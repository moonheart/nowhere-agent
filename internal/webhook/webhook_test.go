package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDeliverPostsPayload(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %s, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-Nowhere-Event") != "run.completed" {
			t.Errorf("event header = %s", r.Header.Get("X-Nowhere-Event"))
		}
		var p RunCompletedPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		got.Store(p)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Options{Timeout: 2 * time.Second, Retries: 0, Logger: testLogger(t)})
	err := n.Deliver(context.Background(), srv.URL, RunCompletedPayload{
		Event:     "run.completed",
		RunID:     "r1",
		SessionID: "s1",
		UserID:    "u1",
		TaskID:    "t1",
		Status:    "done",
		TeamID:    "team1",
		Model:     "deepseek-chat",
		EndedAt:   time.Now().UTC(),
		Summary:   "结果: 全部通过",
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	p, ok := got.Load().(RunCompletedPayload)
	if !ok {
		t.Fatal("no payload received")
	}
	if p.RunID != "r1" || p.Status != "done" || p.Summary != "结果: 全部通过" || p.Model != "deepseek-chat" {
		t.Errorf("payload mismatch: %+v", p)
	}
}

func TestDeliverRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if calls.Load() < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Options{Timeout: 2 * time.Second, Retries: 3, Logger: testLogger(t)})
	if err := n.Deliver(context.Background(), srv.URL, RunCompletedPayload{RunID: "r1", Status: "done"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (first + 2 retries)", calls.Load())
	}
}

func TestDeliverStopsOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	n := New(Options{Timeout: 2 * time.Second, Retries: 3, Logger: testLogger(t)})
	if err := n.Deliver(context.Background(), srv.URL, RunCompletedPayload{RunID: "r1"}); err == nil {
		t.Fatal("want error for 400")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", calls.Load())
	}
}

func TestDeliverEmptyURLNoop(t *testing.T) {
	n := New(Options{Logger: testLogger(t)})
	if err := n.Deliver(context.Background(), "", RunCompletedPayload{RunID: "r1"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

func TestDeliverHonorsCancelledContext(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := New(Options{Timeout: 500 * time.Millisecond, Retries: 0, Logger: testLogger(t)})
	if err := n.Deliver(ctx, srv.URL, RunCompletedPayload{RunID: "r1"}); err == nil {
		t.Fatal("want error for cancelled context")
	}
}
