package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassifyHTTPError413(t *testing.T) {
	err := ClassifyHTTPError(413, "Payload Too Large")
	if err == nil {
		t.Fatal("413 should classify as context overflow")
	}
	if !IsContextOverflow(err) {
		t.Error("IsContextOverflow should match")
	}
}

func TestClassifyHTTPError400ContextMarkers(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`,
		`{"error":"prompt is too long: 200000 tokens > 199000 maximum"}`,
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt_too_long"}}`,
		`reduce the length of the messages`,
	}
	for _, b := range bodies {
		if err := ClassifyHTTPError(400, b); err == nil {
			t.Errorf("400 with body %q should classify as overflow", b)
		}
	}
}

func TestClassifyHTTPErrorNonOverflow(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{400, `{"error":"invalid api key"}`},
		{401, `unauthorized`},
		{429, `rate limit`},
		{500, `internal error`},
	} {
		if err := ClassifyHTTPError(tc.code, tc.body); err != nil {
			t.Errorf("status %d body %q should NOT classify as overflow, got %v", tc.code, tc.body, err)
		}
	}
}

func TestIsContextOverflowUnwraps(t *testing.T) {
	wrapped := fmt.Errorf("stream: %w", &ContextOverflowError{StatusCode: 413})
	if !IsContextOverflow(wrapped) {
		t.Error("should unwrap wrapped ContextOverflowError")
	}
	if IsContextOverflow(errors.New("plain")) {
		t.Error("plain error should not be overflow")
	}
}
