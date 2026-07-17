package provider

import "testing"

type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string { return f.name }
func (f fakeAdapter) Stream(Request) (<-chan Event, error) {
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(fakeAdapter{name: "a"})
	r.Register(fakeAdapter{name: "b"})

	got, err := r.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name() != "a" {
		t.Errorf("got %q", got.Name())
	}

	if len(r.Names()) != 2 {
		t.Errorf("expected 2 names, got %d", len(r.Names()))
	}
}

func TestRegistryGetMissing(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Get("nope"); err == nil {
		t.Error("expected error for unregistered provider")
	}
}
