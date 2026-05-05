package buffer

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuffer_WriteAndRead(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	b.Write([]byte("hello"))
	b.Write([]byte(" world"))

	data, err := b.Read(context.Background(), r, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}

	// Second read with no new data should return empty immediately
	data, err = b.Read(context.Background(), r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %q", string(data))
	}
}

func TestBuffer_ReadWaitsForData(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	done := make(chan struct{})

	go func() {
		time.Sleep(100 * time.Millisecond)
		b.Write([]byte("delayed"))
		close(done)
	}()

	data, err := b.Read(context.Background(), r, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "delayed" {
		t.Fatalf("expected 'delayed', got %q", string(data))
	}
	<-done
}

func TestBuffer_ReadTimeout(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	start := time.Now()
	data, err := b.Read(context.Background(), r, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty, got %q", string(data))
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("returned too fast: %v", elapsed)
	}
}

func TestBuffer_Overwrite(t *testing.T) {
	b := New(32)
	r, _ := b.NewReader()

	// Write 48 bytes into 32-byte buffer — first 16 bytes overwritten
	b.Write([]byte(strings.Repeat("a", 16)))
	b.Write([]byte(strings.Repeat("b", 16)))
	b.Write([]byte(strings.Repeat("c", 16)))

	data, err := b.Read(context.Background(), r, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, strings.Repeat("a", 16)) {
		t.Fatal("expected 'a' chunk to be overwritten")
	}
	if !strings.Contains(s, strings.Repeat("b", 16)) {
		t.Fatalf("expected 'b' chunk to survive, got %q", s)
	}
	if !strings.Contains(s, strings.Repeat("c", 16)) {
		t.Fatalf("expected 'c' chunk to survive, got %q", s)
	}
}

func TestBuffer_CloseWakesReaders(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	done := make(chan struct{})

	go func() {
		data, err := b.Read(context.Background(), r, 10*time.Second)
		if len(data) != 0 {
			t.Errorf("expected empty on close, got %q", string(data))
		}
		if err != io.EOF {
			t.Errorf("expected io.EOF, got %v", err)
		}
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	b.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the reader")
	}
}

func TestBuffer_WriteAfterClose(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()
	b.Write([]byte("before"))
	b.Close()

	err := b.Write([]byte("after"))
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}

	data, err := b.Read(context.Background(), r, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before" {
		t.Fatalf("expected 'before', got %q", string(data))
	}
}

func TestBuffer_ConcurrentReadWrite(t *testing.T) {
	b := New(1024 * 1024)
	r, _ := b.NewReader()
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				b.Write([]byte("w"))
			}
		}(i)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		b.Read(context.Background(), r, 2*time.Second)
	}()

	wg.Wait()
}

func TestBuffer_MultiReaderIndependence(t *testing.T) {
	b := New(1024)
	r1, _ := b.NewReader()
	r2, _ := b.NewReader()

	b.Write([]byte("hello"))

	data1, err := b.Read(context.Background(), r1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	data2, err := b.Read(context.Background(), r2, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if string(data1) != "hello" {
		t.Fatalf("reader 1 expected 'hello', got %q", string(data1))
	}
	if string(data2) != "hello" {
		t.Fatalf("reader 2 expected 'hello', got %q", string(data2))
	}
}

func TestBuffer_MultiReaderSequentialWrite(t *testing.T) {
	b := New(1024)
	r1, _ := b.NewReader()
	r2, _ := b.NewReader()

	b.Write([]byte("chunk1"))
	// r1 reads chunk1
	data1, _ := b.Read(context.Background(), r1, time.Second)
	if string(data1) != "chunk1" {
		t.Fatalf("r1 expected 'chunk1', got %q", string(data1))
	}

	b.Write([]byte("chunk2"))
	// r1 reads only chunk2
	data1, _ = b.Read(context.Background(), r1, time.Second)
	if string(data1) != "chunk2" {
		t.Fatalf("r1 expected 'chunk2', got %q", string(data1))
	}
	// r2 reads both chunk1 and chunk2
	data2, _ := b.Read(context.Background(), r2, time.Second)
	s2 := string(data2)
	if !strings.Contains(s2, "chunk1") || !strings.Contains(s2, "chunk2") {
		t.Fatalf("r2 expected both chunks, got %q", s2)
	}
}

func TestBuffer_Unregister(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	b.Write([]byte("before"))
	b.Unregister(r)

	// Read from unregistered reader should return error
	_, err := b.Read(context.Background(), r, 0)
	if err != ErrReader {
		t.Fatalf("expected ErrReader, got %v", err)
	}
}

func TestBuffer_UnregisterWakesReader(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	done := make(chan error, 1)
	go func() {
		_, err := b.Read(context.Background(), r, 10*time.Second)
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	b.Unregister(r)

	select {
	case err := <-done:
		if err != ErrReader {
			t.Fatalf("expected ErrReader after unregister, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Unregister did not wake the blocked reader")
	}
}

func TestBuffer_HasMore(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	if b.HasMore(r) {
		t.Fatal("expected no data initially")
	}

	b.Write([]byte("data"))
	if !b.HasMore(r) {
		t.Fatal("expected data after write")
	}

	b.Read(context.Background(), r, 0)
	if b.HasMore(r) {
		t.Fatal("expected no data after read")
	}
}

func TestBuffer_InvalidReader(t *testing.T) {
	b := New(1024)
	_, err := b.Read(context.Background(), 999, 0)
	if err != ErrReader {
		t.Fatalf("expected ErrReader, got %v", err)
	}
}

func TestBuffer_ReadCancelledContext(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	data, err := b.Read(ctx, r, 10*time.Second)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("Read should return quickly on ctx cancel, took %v", elapsed)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty data on cancelled ctx, got %q", string(data))
	}
	if err != nil {
		t.Fatalf("expected nil error on cancelled ctx, got %v", err)
	}
}

func TestBuffer_ReadCancelledContextWithData(t *testing.T) {
	b := New(1024)
	r, _ := b.NewReader()

	ctx := context.Background()

	// Write data first, then read with valid ctx — should get data immediately
	b.Write([]byte("hello"))
	data, err := b.Read(ctx, r, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(data))
	}
}

func TestBuffer_NewReaderOnClosed(t *testing.T) {
	b := New(1024)
	b.Close()
	_, err := b.NewReader()
	if err != ErrClosed {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestBuffer_ReadTimeoutReliability(t *testing.T) {
	// Read must return after timeout even when concurrent Writes cause
	// the reader to loop between Wait calls while the AfterFunc fires.
	b := New(1024)
	r, _ := b.NewReader()

	for i := 0; i < 100; i++ {
		done := make(chan struct{})
		go func() {
			data, err := b.Read(context.Background(), r, 50*time.Millisecond)
			if err != nil && err != io.EOF {
				t.Errorf("unexpected error: %v", err)
			}
			_ = data
			close(done)
		}()

		// Concurrent writes create contention: reader wakes, loops,
		// competes for lock — widens the race window for AfterFunc.
		go func() {
			for j := 0; j < 50; j++ {
				b.Write([]byte("x"))
				time.Sleep(time.Millisecond)
			}
		}()

		select {
		case <-done:
			// good — Read returned within timeout
		case <-time.After(2 * time.Second):
			t.Fatalf("Read blocked forever on iteration %d — AfterFunc race", i)
		}
	}
}

func TestBuffer_StressConcurrentReadCloseUnregister(t *testing.T) {
	b := New(1024)
	const rounds = 50
	for i := 0; i < rounds; i++ {
		r, _ := b.NewReader()
		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			b.Read(context.Background(), r, 100*time.Millisecond)
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				b.Write([]byte("stress"))
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(30 * time.Millisecond)
			b.Unregister(r)
		}()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("deadlock in round %d", i)
		}

		// Reset buffer for next round
		b.Close()
		b = New(1024)
	}
}
