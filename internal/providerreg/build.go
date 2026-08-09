package providerreg

import (
	"context"
	"fmt"
	"time"

	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/provider/anthropic"
	"nowhere-agent/internal/provider/openai"
)

// buildAnthropic constructs the Anthropic adapter for a Target. WithEndpoint is
// only applied when the row carries a base URL; otherwise the vendor default
// (the official endpoint) is used. Returns nil only when the key is empty is
// not the case here — vendor dispatch is done by the caller.
func buildAnthropic(t Target, recorder *provider.RawRecorder, streamIdle time.Duration) *anthropic.Adapter {
	var opts []anthropic.Option
	if t.BaseURL != "" {
		opts = append(opts, anthropic.WithEndpoint(t.BaseURL))
	}
	if recorder != nil {
		opts = append(opts, anthropic.WithRawRecorder(recorder))
	}
	if streamIdle > 0 {
		opts = append(opts, anthropic.WithStreamIdleTimeout(streamIdle))
	}
	return anthropic.New(t.APIKey, opts...)
}

func buildOpenAI(t Target, recorder *provider.RawRecorder, streamIdle time.Duration) *openai.Adapter {
	var opts []openai.Option
	if t.BaseURL != "" {
		opts = append(opts, openai.WithEndpoint(t.BaseURL))
	}
	if recorder != nil {
		opts = append(opts, openai.WithRawRecorder(recorder))
	}
	if streamIdle > 0 {
		opts = append(opts, openai.WithStreamIdleTimeout(streamIdle))
	}
	return openai.New(t.APIKey, opts...)
}

// BuildAdapter constructs a provider.Adapter for a resolved Target: vendor and
// base URL from the provider row and the decrypted key from the row, sharing
// the boot-level raw recorder and the stream stall guard. Returns nil for an
// unknown vendor.
func BuildAdapter(t Target, recorder *provider.RawRecorder, streamIdle time.Duration) provider.Adapter {
	switch t.Vendor {
	case VendorAnthropic:
		return buildAnthropic(t, recorder, streamIdle)
	case VendorOpenAI:
		return buildOpenAI(t, recorder, streamIdle)
	default:
		return nil
	}
}

// ListModels fetches the model identifiers the provider's own API serves
// (GET /models on the base URL). It is the "fetch models" action backing the
// admin console: the list is a CANDIDATE set — nothing is written here, the
// caller decides which names to register.
func ListModels(ctx context.Context, t Target, recorder *provider.RawRecorder, streamIdle time.Duration) ([]string, error) {
	switch t.Vendor {
	case VendorAnthropic:
		return buildAnthropic(t, recorder, streamIdle).Models(ctx)
	case VendorOpenAI:
		return buildOpenAI(t, recorder, streamIdle).Models(ctx)
	default:
		return nil, fmt.Errorf("unsupported provider vendor %q", t.Vendor)
	}
}
