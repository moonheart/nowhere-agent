package logging

import "testing"

func TestNewDoesNotPanic(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "bogus", ""} {
		for _, format := range []string{"text", "json", "bogus"} {
			if l := New(lvl, format); l == nil {
				t.Fatalf("New(%q,%q) returned nil", lvl, format)
			}
		}
	}
}
