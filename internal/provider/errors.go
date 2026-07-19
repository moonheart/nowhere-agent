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

// ClassifyHTTPError classifies an HTTP error status+body, returning a
// *ContextOverflowError when the rejection is a context/token limit and nil
// otherwise. Adapters call this on a non-200 response before wrapping the error.
func ClassifyHTTPError(statusCode int, body string) error {
	if statusCode == 413 {
		return &ContextOverflowError{StatusCode: statusCode, Body: body}
	}
	if statusCode == 400 || statusCode == 422 {
		b := strings.ToLower(body)
		for _, marker := range []string{
			"context length", "context_length", "context window",
			"maximum context", "too many tokens", "prompt is too long",
			"prompt_too_long", "max_tokens", "context overflow",
			"reduce the length", "exceeds the context",
		} {
			if strings.Contains(b, marker) {
				return &ContextOverflowError{StatusCode: statusCode, Body: body}
			}
		}
	}
	return nil
}
