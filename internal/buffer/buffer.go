package buffer

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/smallnest/ringbuffer"
)

const DefaultMaxBytes = 1024 * 1024

var (
	ErrClosed = errors.New("buffer: closed")
	ErrReader = errors.New("buffer: invalid reader ID")
)

// Buffer is a thread-safe multi-reader ring buffer for process output.
// Each reader gets its own independent view of the data via a ringbuffer
// that supports overwrite semantics.
type Buffer struct {
	mu      sync.Mutex
	size    int
	readers map[int]*ringbuffer.RingBuffer
	nextID  int
	closed  bool
	cond    *sync.Cond
}

// New creates a Buffer with the given max capacity in bytes.
func New(maxBytes int) *Buffer {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	b := &Buffer{
		size:    maxBytes,
		readers: make(map[int]*ringbuffer.RingBuffer),
	}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// NewReader registers a new independent reader and returns its ID.
// Returns ErrClosed if the buffer is already closed.
func (b *Buffer) NewReader() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, ErrClosed
	}
	id := b.nextID
	b.nextID++
	rb := ringbuffer.New(b.size)
	rb.SetOverwrite(true)
	b.readers[id] = rb
	return id, nil
}

// Unregister removes a reader and wakes any goroutines waiting on it.
func (b *Buffer) Unregister(id int) {
	b.mu.Lock()
	delete(b.readers, id)
	b.mu.Unlock()
	b.cond.Broadcast()
}

// Write appends data to the buffer, broadcasting to all waiting readers.
func (b *Buffer) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	for _, rb := range b.readers {
		rb.Write(data)
	}
	b.cond.Broadcast()
	return nil
}

// Read reads all available data for the given reader.
// If no data is available, it waits up to timeout or until ctx is cancelled.
// Returns (nil, ErrReader) for invalid reader IDs.
// Returns (nil, io.EOF) if the buffer is closed.
func (b *Buffer) Read(ctx context.Context, readerID int, timeout time.Duration) ([]byte, error) {
	b.mu.Lock()
	rb, ok := b.readers[readerID]
	if !ok {
		b.mu.Unlock()
		return nil, ErrReader
	}

	if data := b.drain(rb); data != nil {
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

	for rb.Length() == 0 && !b.closed {
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

	if data := b.drain(rb); data != nil {
		b.mu.Unlock()
		return data, nil
	}

	b.mu.Unlock()
	if b.closed {
		return nil, io.EOF
	}
	return nil, nil
}

// drain reads all available bytes from a ringbuffer. Returns nil if empty.
// Must be called with b.mu held.
func (b *Buffer) drain(rb *ringbuffer.RingBuffer) []byte {
	if length := rb.Length(); length > 0 {
		buf := make([]byte, length)
		n, _ := rb.Read(buf)
		return buf[:n]
	}
	return nil
}

// HasMore returns whether the given reader has unread data.
func (b *Buffer) HasMore(readerID int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	rb, ok := b.readers[readerID]
	if !ok {
		return false
	}
	return rb.Length() > 0
}

// Close marks the buffer as closed and wakes all waiting readers.
func (b *Buffer) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}
