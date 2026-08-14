package provider

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingBody blocks in Read until closed, simulating a hung upstream that
// never sends another byte.
type blockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody() *blockingBody { return &blockingBody{closed: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestStallReaderPassesDataThrough(t *testing.T) {
	r := NewStallReader(io.NopCloser(strings.NewReader("hello")), time.Minute)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q want hello", got)
	}
}

func TestStallReaderDisabledWhenZero(t *testing.T) {
	body := io.NopCloser(strings.NewReader("x"))
	if got := NewStallReader(body, 0); got != body {
		t.Error("timeout<=0 should return the body unchanged (stall detection disabled)")
	}
}

func TestStallReaderTimesOut(t *testing.T) {
	body := newBlockingBody()
	r := NewStallReader(body, 20*time.Millisecond)
	_, err := r.Read(make([]byte, 1))
	var stall *StreamStallError
	if !errors.As(err, &stall) {
		t.Fatalf("err = %v, want *StreamStallError", err)
	}
	// Sticky: subsequent reads report the same terminal error.
	if _, err2 := r.Read(make([]byte, 1)); !errors.As(err2, &stall) {
		t.Errorf("second read err = %v, want the sticky stall error", err2)
	}
}

// A pause between reads (consumer backpressure) must not trip the stall: the
// deadline only counts while a read is pending, so data arriving after a gap
// longer than the timeout still flows. This pins the event-driven watchdog
// semantics — a polled watchdog would spuriously close a live stream here.
func TestStallReaderGapBetweenReadsDoesNotStall(t *testing.T) {
	r := NewStallReader(io.NopCloser(strings.NewReader("hello")), 30*time.Millisecond)
	first := make([]byte, 1)
	if _, err := r.Read(first); err != nil {
		t.Fatalf("first read err = %v", err)
	}
	// No read pending for longer than the timeout.
	time.Sleep(3 * time.Duration(30*time.Millisecond))
	rest := make([]byte, 4)
	if _, err := io.ReadFull(r, rest); err != nil {
		t.Fatalf("read after gap err = %v, want data", err)
	}
	_ = r.Close()
}
