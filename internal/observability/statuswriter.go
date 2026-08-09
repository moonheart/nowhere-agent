package observability

import (
	"net/http"
	"time"
)

// statusWriter is the one response recorder the inbound middleware stack
// shares. It captures the status code, bytes written, and time-to-first-byte
// for a single request, and forwards Write/WriteHeader/Flush/Unwrap to the
// underlying writer. AccessLog and Metrics each wrap one instance (they sit at
// different layers of the stack by design — AccessLog outside the rate limiter
// so it sees 429s, Metrics inside so rejected floods do not churn series — so
// they cannot literally share one instance; sharing the TYPE is what removes
// the duplicated recorder logic every new middleware used to copy).
//
// It satisfies http.Flusher and Unwrap so SSE endpoints that assert
// http.Flusher directly (a type assertion does not traverse Unwrap) keep
// working, and http.ResponseController can reach the underlying writer.
type statusWriter struct {
	http.ResponseWriter
	start  time.Time
	status int
	bytes  int
	ttfb   time.Duration
	wrote  bool
}

func newStatusWriter(w http.ResponseWriter, start time.Time) *statusWriter {
	return &statusWriter{ResponseWriter: w, start: start, status: http.StatusOK}
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.wrote = true
		s.status = code
		s.ttfb = time.Since(s.start)
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Written reports whether the handler committed a status, so Recovery knows a
// 500 can no longer be written (the stream is already underway).
func (s *statusWriter) Written() bool { return s.wrote }

// Flush forwards streaming flushes (SSE asserts http.Flusher directly; a type
// assertion does not traverse Unwrap).
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
