package adminapi

import (
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter that records SetWriteDeadline so a
// test can assert the export path keeps re-arming its write deadline.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline    time.Time
	deadlineSet bool
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadline = t
	d.deadlineSet = true
	return nil
}

// TestRollingDeadlineWriterReArmsPerWrite pins the export stream's rolling
// write deadline: every batch write re-arms it (so a large export is never
// truncated by the server WriteTimeout), while a stalled write still ends the
// response instead of hanging it.
func TestRollingDeadlineWriterReArmsPerWrite(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	wd := rollingDeadlineWriter{w: rec}

	if _, err := wd.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if !rec.deadlineSet {
		t.Fatal("first write did not arm a deadline")
	}
	if left := time.Until(rec.deadline); left <= 0 || left > exportWriteTimeout+time.Second {
		t.Fatalf("write deadline = %v (in %v), want within %v in the future", rec.deadline, left, exportWriteTimeout)
	}

	first := rec.deadline
	time.Sleep(5 * time.Millisecond)
	if _, err := wd.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if !rec.deadline.After(first) {
		t.Errorf("deadline after second write = %v, want refreshed past %v", rec.deadline, first)
	}
	if rec.Body.String() != "firstsecond" {
		t.Errorf("body = %q, want the writes forwarded unchanged", rec.Body.String())
	}
}
