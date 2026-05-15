package buffer

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// DefaultMaxBytes is kept for API compatibility with buffer.New(maxBytes); sizing is no longer enforced.
const DefaultMaxBytes = 1024 * 1024

var (
	ErrClosed = errors.New("buffer: closed")
	ErrReader = errors.New("buffer: invalid reader ID")
)

type readerState struct {
	readPos int64 // next byte offset in master to deliver
}

// Buffer is a thread-safe multi-reader append-only log of process output.
// All readers share one master byte slice; each reader has an independent read cursor.
// Fully consumed prefixes are dropped when compactThreshold is exceeded to bound memory.
type Buffer struct {
	mu                sync.Mutex
	master            []byte
	readers           map[int]*readerState
	nextID            int
	closed            bool
	cond              *sync.Cond
	compactThreshold  int // compact when len(master) > this
	compactMinAdvance int // and min(readPos) >= this
}

// New creates a Buffer. maxBytes is ignored (historical ring capacity); output grows without a fixed cap.
func New(maxBytes int) *Buffer {
	_ = maxBytes // API compatibility; no hard limit on retained history
	b := &Buffer{
		readers:           make(map[int]*readerState),
		compactThreshold:  8 << 20, // 8 MiB before attempting prefix trim
		compactMinAdvance: 1 << 20, // require ≥1 MiB reclaimable prefix
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// NewReader registers a new independent reader that only observes writes after registration.
func (b *Buffer) NewReader() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: int64(len(b.master))}
	return id, nil
}

// NewReaderFromStart registers a reader at the beginning of the retained master buffer
// (full in-memory scrollback for UI reconnect). Caller must drain this reader or it pins old bytes from compaction.
func (b *Buffer) NewReaderFromStart() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: 0}
	return id, nil
}

// NewReaderSeededFrom registers a new reader whose cursor starts at srcReaderID's cursor
// (same logical position — no duplicate copy of backlog).
func (b *Buffer) NewReaderSeededFrom(srcReaderID int) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	src, ok := b.readers[srcReaderID]
	if !ok {
		return 0, ErrReader
	}
	id := b.nextID
	b.nextID++
	b.readers[id] = &readerState{readPos: src.readPos}
	return id, nil
}

func (b *Buffer) Unregister(id int) {
	b.mu.Lock()
	delete(b.readers, id)
	b.mu.Unlock()
	b.cond.Broadcast()
}

// Write appends data for all readers and wakes waiters.
func (b *Buffer) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	b.master = append(b.master, data...)
	b.maybeCompactLocked()
	b.cond.Broadcast()
	return nil
}

// maybeCompactLocked drops a prefix of master that every reader has already passed.
// Caller must hold b.mu.
func (b *Buffer) maybeCompactLocked() {
	if len(b.master) <= b.compactThreshold {
		return
	}
	minPos := int64(len(b.master))
	for _, rs := range b.readers {
		if rs.readPos < minPos {
			minPos = rs.readPos
		}
	}
	if minPos < int64(b.compactMinAdvance) {
		return
	}
	b.master = b.master[minPos:]
	for _, rs := range b.readers {
		rs.readPos -= minPos
	}
}

// Read returns bytes available ahead of the reader's cursor, then advances the cursor.
// If maxBytes > 0, at most that many bytes are returned and the cursor advances by that amount;
// if maxBytes == 0, all bytes from the cursor to the end of the buffer are returned (legacy behavior).
func (b *Buffer) Read(ctx context.Context, readerID int, timeout time.Duration, maxBytes int) ([]byte, error) {
	b.mu.Lock()
	rs, ok := b.readers[readerID]
	if !ok {
		b.mu.Unlock()
		return nil, ErrReader
	}

	if data := b.drainLocked(rs, maxBytes); data != nil {
		b.mu.Unlock()
		return data, nil
	}

	if b.closed {
		b.mu.Unlock()
		return nil, io.EOF
	}

	if timeout <= 0 {
		b.mu.Unlock()
		return nil, nil
	}

	deadline := time.Now().Add(timeout)
	stop := make(chan struct{})
	ctxDone := ctx.Done()
	go func() {
		select {
		case <-time.After(time.Until(deadline)):
			b.cond.Broadcast()
		case <-ctxDone:
			b.cond.Broadcast()
		case <-stop:
		}
	}()
	defer close(stop)

	for rs.readPos >= int64(len(b.master)) && !b.closed {
		if time.Until(deadline) <= 0 {
			b.mu.Unlock()
			return nil, nil
		}
		if ctxDone != nil {
			select {
			case <-ctxDone:
				b.mu.Unlock()
				return nil, nil
			default:
			}
		}
		b.cond.Wait()
		if _, ok := b.readers[readerID]; !ok {
			b.mu.Unlock()
			return nil, ErrReader
		}
	}

	if data := b.drainLocked(rs, maxBytes); data != nil {
		b.mu.Unlock()
		return data, nil
	}

	b.mu.Unlock()
	if b.closed {
		return nil, io.EOF
	}
	return nil, nil
}

// drainLocked copies up to one slice from readPos forward and advances readPos. b.mu held.
func (b *Buffer) drainLocked(rs *readerState, maxBytes int) []byte {
	end := int64(len(b.master))
	if rs.readPos >= end {
		return nil
	}
	endPos := end
	if maxBytes > 0 {
		if lim := rs.readPos + int64(maxBytes); lim < endPos {
			endPos = lim
		}
	}
	s := b.master[rs.readPos:endPos]
	out := make([]byte, len(s))
	copy(out, s)
	rs.readPos = endPos
	return out
}

func (b *Buffer) HasMore(readerID int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs, ok := b.readers[readerID]
	if !ok {
		return false
	}
	return rs.readPos < int64(len(b.master))
}

// Len returns the current retained master size in bytes.
func (b *Buffer) Len() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.master))
}

// ByteRange returns a copy of master[start:start+n] with n = min(max, len-start), and total = len(master).
// No reader cursors are advanced.
func (b *Buffer) ByteRange(start int64, max int) (out []byte, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total = int64(len(b.master))
	if start < 0 {
		start = 0
	}
	if start >= total || max <= 0 {
		return nil, total
	}
	n := int64(max)
	if start+n > total {
		n = total - start
	}
	if n <= 0 {
		return nil, total
	}
	out = make([]byte, n)
	copy(out, b.master[start:start+n])
	return out, total
}

func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}
