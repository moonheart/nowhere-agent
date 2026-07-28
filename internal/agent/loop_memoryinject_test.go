package agent

import (
	"context"
	"testing"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/toolruntime"
)

// fakeInjector records the view it was given and returns a fixed extra message.
type fakeInjector struct {
	extra    []provider.Message
	gotView  []provider.Message
	gotSess  string
	injected bool
}

func (f *fakeInjector) Inject(_ context.Context, sessionID string, view []provider.Message) ([]provider.Message, error) {
	f.gotSess = sessionID
	f.gotView = view
	f.injected = true
	return f.extra, nil
}

// TestAttemptInjectsMemoryIntoOutgoingView pins the incremental-injection seam:
// the injector's extra message reaches the provider's request (appended to the
// tail), but does NOT enter the loop's produced messages (so it is never
// persisted).
func TestAttemptInjectsMemoryIntoOutgoingView(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{textResponse("hi")}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})

	memMsg := provider.TextMessage(provider.RoleUser, "[背景记忆] prefers dark mode")
	inj := &fakeInjector{extra: []provider.Message{memMsg}}
	loop.Use(&MemoryInjectMW{Injector: inj, SessionID: "sess-1"})

	history := []provider.Message{provider.TextMessage(provider.RoleUser, "hello")}
	produced, err := loop.Run(context.Background(), history, &memEmitter{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !inj.injected {
		t.Fatal("injector not called")
	}
	if inj.gotSess != "sess-1" {
		t.Errorf("injector session = %q want sess-1", inj.gotSess)
	}

	// The provider's request carries the history + the injected memory message.
	if len(p.requests) != 1 {
		t.Fatalf("requests = %d want 1", len(p.requests))
	}
	sent := p.requests[0].Messages
	if len(sent) != 2 {
		t.Fatalf("sent messages = %d want 2 (history + injected): %+v", len(sent), sent)
	}
	last := sent[len(sent)-1]
	if last.Content[0].Text != "[背景记忆] prefers dark mode" {
		t.Errorf("last sent message = %q, want the injected memory", last.Content[0].Text)
	}

	// produced must NOT contain the injected message (only the assistant reply).
	for _, m := range produced {
		for _, b := range m.Content {
			if b.Text == "[背景记忆] prefers dark mode" {
				t.Error("injected memory leaked into produced (would be persisted)")
			}
		}
	}
}

// TestAttemptSkipsInjectionWhenEmpty: a nil injector result leaves the view
// unchanged (no trailing message appended).
func TestAttemptSkipsInjectionWhenEmpty(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{textResponse("hi")}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	loop.Use(&MemoryInjectMW{Injector: &fakeInjector{extra: nil}, SessionID: "sess-1"})

	history := []provider.Message{provider.TextMessage(provider.RoleUser, "hello")}
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(p.requests[0].Messages); got != 1 {
		t.Errorf("sent messages = %d want 1 (no injection when empty)", got)
	}
}

// TestAttemptNoInjector: a nil injector is a no-op (unauthenticated/direct path).
func TestAttemptNoInjector(t *testing.T) {
	p := &scriptProvider{script: [][]provider.Event{textResponse("hi")}}
	loop := New(p, toolruntime.NewRegistry(), Config{Model: "m", MaxTokens: 100})
	history := []provider.Message{provider.TextMessage(provider.RoleUser, "hello")}
	if _, err := loop.Run(context.Background(), history, &memEmitter{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(p.requests[0].Messages); got != 1 {
		t.Errorf("sent messages = %d want 1 (no injector)", got)
	}
}
