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
