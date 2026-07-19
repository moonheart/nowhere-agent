package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"nowhere-agent/internal/provider"
)

const defaultEndpoint = "https://api.openai.com/v1/chat/completions"

// Adapter implements provider.Adapter for OpenAI-compatible chat.completions.
type Adapter struct {
	apiKey     string
	endpoint   string
	httpClient *http.Client
	recorder   *provider.RawRecorder
}

// Option customizes the Adapter.
type Option func(*Adapter)

// WithEndpoint overrides the API endpoint (also enables OpenAI-compatible proxies).
func WithEndpoint(url string) Option { return func(a *Adapter) { a.endpoint = url } }

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(a *Adapter) { a.httpClient = c } }

// WithRawRecorder records raw request/response wire bytes (for debugging).
func WithRawRecorder(r *provider.RawRecorder) Option {
	return func(a *Adapter) { a.recorder = r }
}

// New creates an OpenAI adapter.
func New(apiKey string, opts ...Option) *Adapter {
	a := &Adapter{apiKey: apiKey, endpoint: defaultEndpoint, httpClient: http.DefaultClient, recorder: provider.NewRawRecorder("")}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Name returns the provider identifier.
func (a *Adapter) Name() string { return "openai" }

// Stream starts a streaming generation and returns canonical events. The
// caller's ctx governs cancellation: cancelling it aborts the HTTP request.
func (a *Adapter) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	apiReq, err := buildRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+a.apiKey)

	resp, err := a.httpClient.Do(httpReq)
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
		return nil, fmt.Errorf("openai status %d: %s", resp.StatusCode, string(b))
	}

	// Record the raw request/response wire bytes (tee'd so streaming continues
	// while the body is captured as read).
	respSink := a.recorder.Exchange(a.Name(), body)
	recorded := io.TeeReader(resp.Body, respSink)

	out := make(chan provider.Event, 16)
	go streamEvents(teeCloser{recorded, resp.Body, respSink}, out)
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
	_ = tc.sink.Close()
	return err
}

// streamEvents reads the SSE body and emits canonical events until EOF.
func streamEvents(body io.ReadCloser, out chan<- provider.Event) {
	defer close(out)
	defer body.Close()

	dec := newStreamDecoder()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataBuf bytes.Buffer

	flush := func() {
		if dataBuf.Len() == 0 {
			return
		}
		for _, ev := range dec.feed(dataBuf.Bytes()) {
			out <- ev
		}
		dataBuf.Reset()
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if len(line) >= 6 && line[:6] == "data: " {
			dataBuf.WriteString(line[6:])
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		out <- provider.Event{Type: provider.EventError, Err: err}
	}
}
