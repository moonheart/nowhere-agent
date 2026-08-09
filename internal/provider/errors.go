package provider

import (
	"errors"
	"strconv"
	"strings"
)

// ContextOverflowError marks a provider rejection caused by the request being
// too large for the model's context window (HTTP 413, or a 400 whose body names
// a context/token/prompt-length limit). The agent loop treats it as a signal to
// shrink the working view and retry, rather than a fatal error (design D7).
type ContextOverflowError struct {
	StatusCode int
	Body       string
}

func (e *ContextOverflowError) Error() string {
	return "context overflow (status " + strconv.Itoa(e.StatusCode) + "): " + e.Body
}

// IsContextOverflow reports whether err is a context-overflow rejection,
// unwrapping as needed.
func IsContextOverflow(err error) bool {
	var coe *ContextOverflowError
	return errors.As(err, &coe)
}

// overflowMarkers are the lower-cased substrings that identify a provider
// rejection as a context/token limit, shared by the HTTP-status and the
// mid-stream classifiers.
var overflowMarkers = []string{
	"context length", "context_length", "context window",
	"maximum context", "too many tokens", "prompt is too long",
	"prompt_too_long", "max_tokens", "context overflow",
	"reduce the length", "exceeds the context",
}

// ClassifyHTTPError classifies an HTTP error status+body, returning a
// *ContextOverflowError when the rejection is a context/token limit and nil
// otherwise. Adapters call this on a non-200 response before wrapping the error.
func ClassifyHTTPError(statusCode int, body string) error {
	if statusCode == 413 {
		return &ContextOverflowError{StatusCode: statusCode, Body: body}
	}
	if statusCode == 400 || statusCode == 422 {
		if matchOverflowMarker(body) {
			return &ContextOverflowError{StatusCode: statusCode, Body: body}
		}
	}
	return nil
}

// ClassifyStreamError classifies an error that surfaced MID-STREAM (an SSE
// error envelope after the 200 OK), where no HTTP status is available. It
// returns a *ContextOverflowError when the message names a context/token
// limit, so the loop's overflow fallback treats a mid-stream overflow exactly
// like an initial-response one. Nil when the message matches no marker.
func ClassifyStreamError(message string) error {
	if matchOverflowMarker(message) {
		return &ContextOverflowError{StatusCode: 0, Body: message}
	}
	return nil
}

func matchOverflowMarker(body string) bool {
	b := strings.ToLower(body)
	for _, marker := range overflowMarkers {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}
