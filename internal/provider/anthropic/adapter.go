package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"nowhere-agent/internal/provider"
)

const defaultEndpoint = "https://api.anthropic.com/v1"

// Adapter implements provider.Adapter for the Anthropic Messages API.
type Adapter struct {
	apiKey      string
	endpoint    string // API base URL (root), e.g. https://api.anthropic.com/v1
	anthroVer   string
	httpClient  *http.Client
	recorder    *provider.RawRecorder
	retry       provider.RetryPolicy
	idleTimeout time.Duration
}

// Option customizes the Adapter.
type Option func(*Adapter)

// WithEndpoint sets the API base URL. Accepts either a bare base
// ("https://api.anthropic.com/v1", "https://proxy.example.com/v1") or the full
// legacy endpoint (".../v1/messages"); both are normalized to the base so chat
// and model-list calls share one root.
func WithEndpoint(url string) Option {
	return func(a *Adapter) { a.endpoint = provider.NormalizeBase(url) }
}

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(a *Adapter) { a.httpClient = c } }

// WithRawRecorder records raw request/response wire bytes (for debugging).
func WithRawRecorder(r *provider.RawRecorder) Option {
	return func(a *Adapter) { a.recorder = r }
}

// WithRetry overrides the transient-failure retry policy (default 3 attempts).
func WithRetry(p provider.RetryPolicy) Option { return func(a *Adapter) { a.retry = p } }

// WithStreamIdleTimeout sets the stall detector: if no SSE bytes arrive for
// the given duration, the stream fails with a *provider.StreamStallError
// instead of blocking until the outer context is cancelled. <=0 disables.
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(a *Adapter) { a.idleTimeout = d }
}

// New creates an Anthropic adapter.
func New(apiKey string, opts ...Option) *Adapter {
	a := &Adapter{
		apiKey:     apiKey,
		endpoint:   defaultEndpoint,
		anthroVer:  "2023-06-01",
		httpClient: http.DefaultClient,
		recorder:   provider.NewRawRecorder(""),
		retry:      provider.DefaultRetryPolicy(),
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name returns the provider identifier.
func (a *Adapter) Name() string { return "anthropic" }

// messagesEndpoint returns the concrete Messages URL for the base.
func (a *Adapter) messagesEndpoint() string {
	return provider.ResolveEndpoint(a.endpoint, provider.EndpointMsg)
}

// modelsEndpoint returns the concrete GET /models URL for the base.
func (a *Adapter) modelsEndpoint() string {
	return provider.ResolveEndpoint(a.endpoint, provider.EndpointModels)
}

// Models lists the model identifiers the API serves (GET /models on the base
// URL), used by the admin console's "fetch models" action to seed the registry.
func (a *Adapter) Models(ctx context.Context) ([]string, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.modelsEndpoint(), nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", a.anthroVer)
	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	names := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	return names, nil
}

// Stream starts a streaming generation and returns canonical events. The
// caller's ctx governs cancellation: cancelling it aborts the HTTP request.
func (a *Adapter) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Send with transient-failure retry (429/5xx/529 + network errors) using
	// exponential backoff. A fresh request is built per attempt so the body
	// reader is re-seeked. Context overflow (a non-retryable 4xx) falls through
	// to the classification below.
	resp, err := provider.DoWithRetry(ctx, a.retry, func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.messagesEndpoint(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("x-api-key", a.apiKey)
		httpReq.Header.Set("anthropic-version", a.anthroVer)
		return a.httpClient.Do(httpReq)
	})
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// Surface context-overflow as a typed error so the loop can shrink the
		// working view and retry instead of failing the run (design D7).
		if ov := provider.ClassifyHTTPError(resp.StatusCode, string(b)); ov != nil {
			return nil, ov
		}
		return nil, fmt.Errorf("anthropic status %d: %s", resp.StatusCode, string(b))
	}

	// Record the raw request/response wire bytes. The response is tee'd so the
	// SSE stream is captured as it is read, without buffering the whole body.
	respSink := a.recorder.Exchange(a.Name(), body)
	recorded := io.TeeReader(resp.Body, respSink)

	out := make(chan provider.Event, 16)
	go streamEvents(ctx, provider.NewStallReader(teeCloser{recorded, resp.Body, respSink}, a.idleTimeout), out)
	return out, nil
}

// teeCloser couples the tee'd reader with the underlying body and the
// recording sink so closing finalizes both.
type teeCloser struct {
	r    io.Reader
	body io.Closer
	sink io.Closer
}

func (tc teeCloser) Read(p []byte) (int, error) { return tc.r.Read(p) }
func (tc teeCloser) Close() error {
	err := tc.body.Close()
	_ = tc.sink.Close() // finalize the recorded response even if body close failed
	return err
}

// streamEvents reads the SSE body and emits canonical events until EOF. Every
// send is ctx-aware: the loop's consumer stops reading the moment the run is
// cancelled, so a full buffer must not leave this goroutine blocked forever —
// it exits and the deferred body.Close() tears the HTTP stream down.
func streamEvents(ctx context.Context, body io.ReadCloser, out chan<- provider.Event) {
	defer close(out)
	defer body.Close()
	// A panic while decoding the provider stream must not crash the process.
	// Declared last so it runs first (LIFO): out is still open, so the error
	// event is delivered before close(out).
	defer func() {
		if p := recover(); p != nil {
			select {
			case out <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("stream panic: %v", p)}:
			case <-ctx.Done():
			}
		}
	}()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataBuf bytes.Buffer

	// flush decodes the accumulated data payload and sends the event, returning
	// false when the run was cancelled so the loop unwinds.
	flush := func() bool {
		if dataBuf.Len() == 0 {
			return true
		}
		if ev, ok := decodeEvent(dataBuf.Bytes()); ok {
			select {
			case out <- ev:
			case <-ctx.Done():
				return false
			}
		}
		dataBuf.Reset()
		return true
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" { // blank line = event boundary
			if !flush() {
				return
			}
			continue
		}
		if len(line) >= 6 && line[:6] == "data: " {
			dataBuf.WriteString(line[6:])
		}
		// ignore "event:" lines; the payload type field discriminates.
	}
	if !flush() {
		return
	}

	if err := scanner.Err(); err != nil {
		select {
		case out <- provider.Event{Type: provider.EventError, Err: err}:
		case <-ctx.Done():
		}
	}
}
