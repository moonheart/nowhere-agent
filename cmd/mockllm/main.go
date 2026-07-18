// Command mockllm is a tiny OpenAI-compatible chat.completions stub for
// deterministic end-to-end testing without a real LLM. It serves
// POST /v1/chat/completions (and /chat/completions) as a throttled SSE stream
// of reasoning_content + content deltas, so a run stays in-flight long enough
// to exercise cancel / disconnect paths.
//
// Point the server at it with:
//
//	LLM_PROVIDER=openai LLM_BASE_URL=http://localhost:9090/v1/chat/completions LLM_API_KEY=mock
//
// Flags: -addr listen address, -delay per-chunk delay, -chunks number of
// chunks per phase (reasoning then text). The stream honours client
// disconnect (request context) so cancel propagation can be tested.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	delay := flag.Duration("delay", 30*time.Millisecond, "delay between streamed chunks")
	chunks := flag.Int("chunks", 400, "number of text chunks to stream (reasoning uses half)")
	flag.Parse()

	mux := http.NewServeMux()
	h := &handler{delay: *delay, chunks: *chunks}
	mux.HandleFunc("/v1/chat/completions", h.serve)
	mux.HandleFunc("/chat/completions", h.serve)

	log.Printf("mockllm listening on %s (delay=%s chunks=%d)", *addr, *delay, *chunks)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

type handler struct {
	delay  time.Duration
	chunks int
}

// chunkJSON is one OpenAI streaming delta. Reasoning and content are emitted
// as separate frames like DeepSeek's reasoning models.
func chunkJSON(reasoning, content string, finish string) string {
	delta := `"role":"assistant"`
	if reasoning != "" {
		delta += `,"reasoning_content":` + jsonStr(reasoning)
	}
	if content != "" {
		delta += `,"content":` + jsonStr(content)
	}
	fin := "null"
	if finish != "" {
		fin = jsonStr(finish)
	}
	return `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1,"model":"mock",` +
		`"choices":[{"index":0,"delta":{` + delta + `},"finish_reason":` + fin + `}]}`
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no streaming", http.StatusInternalServerError)
		return
	}

	send := func(s string) bool {
		select {
		case <-r.Context().Done():
			return false // client went away / cancelled
		default:
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", s); err != nil {
			return false
		}
		fl.Flush()
		if h.delay > 0 {
			select {
			case <-r.Context().Done():
				return false
			case <-time.After(h.delay):
			}
		}
		return true
	}

	// Reasoning phase.
	for i := 0; i < h.chunks/2; i++ {
		if !send(chunkJSON(fmt.Sprintf("reasoning-%d ", i), "", "")) {
			return
		}
	}
	// Answer phase.
	for i := 0; i < h.chunks; i++ {
		if !send(chunkJSON("", fmt.Sprintf("word-%d ", i), "")) {
			return
		}
	}
	// Finish.
	send(chunkJSON("", "", "stop"))
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}
