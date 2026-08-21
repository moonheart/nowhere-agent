package chatapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/httpx"
	"nowhere-agent/internal/provider"
)

// emitter.go — the ui-message-stream protocol encoder: sseEmitter (the
// agent.Emitter over an SSE response) plus the payload decoders its frames
// round-trip through. Transport-only: nothing here knows about sessions,
// runs, or persistence — that orchestration lives in handler.go / attach.go.

// sseEmitter adapts agent.Emitter to write ui-message-stream frames live.
type sseEmitter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	mu          sync.Mutex
	msgID       string
	textID      string
	textStarted bool
	thinkID     string
	thinkOpen   bool
	// writeErr latches the first failed write (e.g. client disconnected), so
	// subsequent Emits report it and the loop unwinds instead of writing into
	// the void while the run leaks.
	writeErr error
	// usageIn/usageOut hold the run's reported token usage, stashed from a
	// KindUsage event, so finish() reports real counts instead of zeros.
	// cacheRead/cacheWrite carry the prompt-prefix cache hits (cache write is
	// Anthropic-only). They ride a separate data-usage frame so the client can
	// show cache detail the finish frame's input/output doesn't carry.
	usageIn    int
	usageOut   int
	cacheRead  int
	cacheWrite int
	// toolStarted tracks tool-call ids whose tool-call-start frame has been
	// written (a streaming KindToolArgs opens the block; the later KindToolUse
	// must not start it again). argsStreamed marks ids whose args arrived via
	// incremental tool-call-delta frames, so KindToolUse skips re-sending the
	// full input as one duplicate delta.
	toolStarted  map[string]bool
	argsStreamed map[string]bool
	// finishReason latches the run's terminal ui-message-stream finish reason
	// ("error" on KindError, "other" on KindCancelled, "length" when the final
	// step was a non-continued max_tokens truncation). Empty means unset; finish()
	// resolves it to "stop" or, at settle time, the run's terminal status.
	finishReason string
	// lastStepReason/lastStepContinued record the most recent finish-step so a
	// following terminal KindError can be classified as a truncation ("length")
	// rather than a generic error when the final step hit max_tokens without
	// continuing (the loop emits that step-finish before the terminal error).
	lastStepReason    string
	lastStepContinued bool
	// refreshDeadline re-arms the rolling per-write deadline before each frame
	// write (see writeStreamHeaders): a stalled write is a dead client and ends
	// the stream instead of wedging the attach loop. Nil in tests / emitters
	// built without a response controller.
	refreshDeadline func()
}

// streamWriteTimeout is the rolling per-write deadline for SSE frames: long
// enough that a live frame write never hits it, short enough that a half-open
// client's blocked write ends the stream instead of hanging it (and, with a
// Redis broker, feeding the slow-consumer busy loop) forever.
const streamWriteTimeout = 30 * time.Second

// heartbeatInterval is how often the attach loop writes an SSE comment frame
// (": ping\n\n") while its run is silent. Comment frames are invisible to
// EventSource and assistant-ui decoders, but they keep the connection alive
// across idle-cutoff proxies (nginx proxy_read_timeout defaults to 60s) and
// refresh the rolling write deadline, so a long silent tool call (run_command
// can run for minutes) never drops the stream while the run continues
// headless. Must stay well below both the proxy cutoff and streamWriteTimeout.
const heartbeatInterval = 20 * time.Second

// newSSEEmitter builds the production emitter over w, wiring the rolling write
// deadline refresh that writeStreamHeaders armed. Tests build sseEmitter
// literals directly (no deadline refresh, which is a no-op for their recorders).
func newSSEEmitter(w http.ResponseWriter, flusher http.Flusher, msgID, textID, thinkID string) *sseEmitter {
	e := &sseEmitter{w: w, flusher: flusher, msgID: msgID, textID: textID, thinkID: thinkID}
	e.refreshDeadline = func() {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	}
	return e
}

// writeStreamHeaders writes the SSE headers for a ui-message-stream response and
// reports whether the connection supports streaming.
func writeStreamHeaders(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	if _, ok := w.(http.Flusher); !ok {
		httpx.Error(w, http.StatusInternalServerError, "streaming unsupported")
		return false
	}
	// Rolling write deadline for this streaming response: the server's
	// WriteTimeout would otherwise abort a long-running SSE stream (an agent
	// run can last far longer than a normal response) mid-run. Instead of
	// clearing the deadline entirely — which lets a half-open client block a
	// frame write forever, wedging the attach loop and (with a Redis broker)
	// feeding a slow-consumer busy loop — arm a rolling deadline that every
	// frame write refreshes: a stalled write is a dead client and must end the
	// stream, while a live stream never hits it. Best-effort — a server
	// without deadline support is unaffected. Non-streaming endpoints keep the
	// server's WriteTimeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	return true
}

// writeSSEError streams a single error frame + finish, for failures after the
// run may have started but before/without a clean attach.
func writeSSEError(w http.ResponseWriter, msg string) {
	if !writeStreamHeaders(w) {
		return
	}
	flusher := w.(http.Flusher)
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
	emitter.Emit(context.Background(), agent.KindError, msg)
	emitter.finish()
}

// Emit implements agent.Emitter, streaming frames as the loop produces them.
// It honours ctx cancellation and reports write failures so the loop unwinds
// (and the run settles) when the client disconnects mid-run, rather than
// blocking forever on a dead connection.
func (e *sseEmitter) Emit(ctx context.Context, kind agent.EventKind, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.writeErr != nil {
		return e.writeErr
	}

	switch kind {
	case agent.KindRunning:
		// A run started: broadcast a transient lifecycle frame so every attached
		// client (not just the one that submitted) sees the session go running.
		e.writeRunStatus("running")
	case agent.KindThinking:
		if !e.thinkOpen {
			e.write(chunk{"type": "reasoning-start", "id": e.thinkID})
			e.thinkOpen = true
		}
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "reasoning-delta", "delta": s})
		}
	case agent.KindText:
		if e.thinkOpen {
			e.write(chunk{"type": "reasoning-end"})
			e.thinkOpen = false
		}
		if !e.textStarted {
			e.write(chunk{"type": "text-start", "id": e.textID})
			e.textStarted = true
		}
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "text-delta", "id": e.textID, "textDelta": s})
		}
	case agent.KindStepStart:
		// A new think→tool step (multi-iteration run). No messageId — the decoder
		// falls back to the current message id.
		e.write(chunk{"type": "start-step"})
	case agent.KindStepFinish:
		// A step closed: record it for terminal-reason classification, then emit a
		// finish-step frame with the step's real usage and isContinued flag.
		if se, ok := stepEvent(payload); ok {
			e.lastStepReason = se.FinishReason
			e.lastStepContinued = se.IsContinued
			in, out := 0, 0
			if se.Usage != nil {
				in, out = se.Usage.InputTokens, se.Usage.OutputTokens
			}
			e.write(chunk{
				"type":         "finish-step",
				"finishReason": se.FinishReason,
				"usage":        map[string]any{"inputTokens": in, "outputTokens": out},
				"isContinued":  se.IsContinued,
			})
		}
	case agent.KindToolArgs:
		// Incremental tool-call arguments as the model streams them. Open the
		// block (id + name) the first time, then forward each fragment as a
		// tool-call-delta so the client renders a large input as it generates.
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			delta, _ := m["delta"].(string)
			e.writeToolCallStart(id, name)
			if delta != "" {
				e.write(chunk{"type": "tool-call-delta", "toolCallId": id, "argsText": delta})
				e.argsStreamed[id] = true
			}
		}
	case agent.KindToolUse:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			e.writeToolCallStart(id, name)
			// assistant-ui streams tool args via tool-call-delta frames (the start
			// frame carries only id+name). When the args already streamed as
			// incremental deltas (KindToolArgs) they're complete on the client, so
			// don't re-send the full input as one duplicate delta. When they did
			// NOT stream — the no-broker direct path, or a provider that closed the
			// stream without emitting args deltas — emit the full input here as one
			// delta so the live UI still shows the arguments (otherwise it renders
			// "{}" until reload; the history path marshals ToolInput separately and
			// is unaffected either way).
			if !e.argsStreamed[id] {
				if input, ok := m["input"]; ok && input != nil {
					if data, err := json.Marshal(input); err == nil {
						e.write(chunk{"type": "tool-call-delta", "toolCallId": id, "argsText": string(data)})
					}
				}
			}
			e.write(chunk{"type": "tool-call-end", "toolCallId": id})
		}
	case agent.KindToolResult:
		if m, ok := payload.(map[string]any); ok {
			id, _ := m["tool_use_id"].(string)
			isErr, _ := m["is_error"].(bool)
			e.write(chunk{"type": "tool-result", "toolCallId": id, "result": m["content"], "isError": isErr})
		}
	case agent.KindSubagent:
		// Subagent progress: a transient data frame the client routes to the run
		// panel (via onData), never added to the message content.
		if m, ok := payload.(map[string]any); ok {
			e.write(chunk{"type": "data-subagent", "data": m, "transient": true})
		}
	case agent.KindInterrupt:
		// Client-interaction prompt (general interrupt): stream the suspended call
		// to the client as a data-interaction frame. One frame for every kind —
		// approval (yes/no), ask_user (question card), client_tool (auto-execute).
		// The loop generated the interaction's ID when it detected the gate
		// (LangGraph-style), so the frame carries it — the card POSTs its verdict
		// with no refresh or lookup. Transient: it drives UI, not the message record.
		interaction, ok := interactionPayload(payload)
		if !ok {
			break
		}
		kind := interaction.Kind
		if kind == "" {
			kind = "approval"
		}
		args := interaction.Input
		if args == nil {
			args = map[string]any{}
		}
		e.write(chunk{"type": "data-interaction", "data": map[string]any{
			"interactionId": interaction.ID,
			"approvalId":    interaction.ID, // legacy alias for clients still reading it
			"kind":          kind,
			"toolCallId":    interaction.ToolCallID,
			"toolName":      interaction.ToolName,
			"args":          args,
		}, "transient": true})
	case agent.KindGenerativeUI:
		// Agent-driven UI a tool result declared: a durable (non-transient) data
		// frame so the client's message accumulates a data part and history
		// reloads re-render it. Shape: {type, data:{spec}}; the client matches
		// the data part by name "generative-ui".
		if m, ok := payload.(map[string]any); ok {
			if spec, ok := m["spec"]; ok {
				e.write(chunk{"type": "data-generative-ui", "data": map[string]any{"spec": spec}})
			}
		}
	case agent.KindDone:
		e.writeRunStatus("done")
	case agent.KindUsage:
		// Stash the run's token usage so finish() can report real counts. Also
		// emit a data-usage frame carrying the full breakdown (incl. cache hits)
		// so the client can render token/cache detail; it's a durable (non-
		// transient) data frame so it lands in message metadata and survives a
		// history reload.
		if u, ok := usageTokens(payload); ok {
			e.usageIn, e.usageOut = u.InputTokens, u.OutputTokens
			e.cacheRead, e.cacheWrite = u.CacheReadTokens, u.CacheWriteTokens
			e.write(chunk{"type": "data-usage", "data": map[string]any{
				"inputTokens":      u.InputTokens,
				"outputTokens":     u.OutputTokens,
				"cacheReadTokens":  u.CacheReadTokens,
				"cacheWriteTokens": u.CacheWriteTokens,
			}})
		}
	case agent.KindError:
		if s, ok := payload.(string); ok {
			e.write(chunk{"type": "error", "errorText": s})
		}
		// Latch the terminal reason. A final step that hit max_tokens without
		// continuing is a truncation ("length"), not a generic failure — the loop
		// emits that non-continued length finish-step just before this error.
		if e.lastStepReason == "length" && !e.lastStepContinued {
			e.finishReason = "length"
		} else {
			e.finishReason = "error"
		}
		e.writeRunStatus("failed")
	case agent.KindCancelled:
		// Close any open block, then flag the run cancelled via a transient data
		// frame so attached clients (and reconnects) can show it stopped early.
		// finish() still terminates the message normally. The spec's FinishReason
		// union has no "cancelled", so the honest member is "other" (not "error" —
		// an intentional stop is not a failure).
		if e.thinkOpen {
			e.write(chunk{"type": "reasoning-end"})
			e.thinkOpen = false
		}
		if e.textStarted {
			e.write(chunk{"type": "text-end", "id": e.textID})
			e.textStarted = false
		}
		e.finishReason = "other"
		e.writeRunStatus("cancelled")
	}
	return e.writeErr
}

// writeRunStatus emits a transient data-run lifecycle frame. Attached clients
// use these to sync run state across tabs/devices (start, terminal status)
// without touching the message content.
func (e *sseEmitter) writeRunStatus(status string) {
	e.write(chunk{"type": "data-run", "data": map[string]any{"status": status}, "transient": true})
}

// finish closes the text block, sends finish + [DONE] using the latched
// terminal reason (or "stop").
func (e *sseEmitter) finish() {
	e.finishWithReason("")
}

// finishWithReason closes the message with an explicit terminal reason. An empty
// reason falls back to the latched finishReason, then to "stop". The reason is
// what the assistant-ui accumulator turns into the message status: only
// "stop"/"unknown" render as complete, so a failed/truncated/cancelled run must
// NOT finish "stop" (that would show a cut-off answer as a clean completion).
func (e *sseEmitter) finishWithReason(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.thinkOpen {
		e.write(chunk{"type": "reasoning-end"})
		e.thinkOpen = false
	}
	if e.textStarted {
		e.write(chunk{"type": "text-end", "id": e.textID})
	}
	if reason == "" {
		reason = e.finishReason
	}
	if reason == "" {
		reason = "stop"
	}
	e.write(chunk{
		"type":         "finish",
		"finishReason": reason,
		"usage":        map[string]any{"inputTokens": e.usageIn, "outputTokens": e.usageOut},
	})
	e.writeRaw("data: [DONE]\n\n")
	e.flusher.Flush()
}

// writeToolCallStart emits a tool-call-start frame for a call at most once,
// guarding against the double-open a streaming KindToolArgs (which opens the
// block) followed by the block-stop KindToolUse (which closes it) would cause.
// Callers hold e.mu. The tracking maps are initialized lazily because emitters
// are built as struct literals at several call sites.
func (e *sseEmitter) writeToolCallStart(id, name string) {
	if id == "" {
		return
	}
	if e.toolStarted == nil {
		e.toolStarted = map[string]bool{}
		e.argsStreamed = map[string]bool{}
	}
	if e.toolStarted[id] {
		return
	}
	e.toolStarted[id] = true
	e.write(chunk{"type": "tool-call-start", "id": id, "toolCallId": id, "toolName": name})
}

func (e *sseEmitter) write(c chunk) {
	e.writeRaw(sseFrame(c))
	e.flusher.Flush()
}

// ping writes an SSE comment frame (": ping\n\n"). Comment lines carry no
// event data, so EventSource and assistant-ui decoders ignore them entirely;
// the frame's only job is keeping the connection alive while the run is
// silent (see heartbeatInterval) and re-arming the rolling write deadline.
func (e *sseEmitter) ping() {
	e.writeRaw(": ping\n\n")
	e.flusher.Flush()
}

func (e *sseEmitter) writeRaw(s string) {
	if e.writeErr != nil {
		return
	}
	if e.refreshDeadline != nil {
		e.refreshDeadline()
	}
	if _, err := e.w.Write([]byte(s)); err != nil {
		e.writeErr = err
	}
}

// usageTokens extracts the full token usage (input/output + cache read/write)
// from a KindUsage payload, tolerating both a provider.Usage value (the loop's
// direct-path emit) and a decoded JSON object (the broker/replay path, where
// the payload round-trips through storage as snake_case JSON). Returns ok=false
// when no token keys are present, so a bad payload never clobbers a prior value.
func usageTokens(payload any) (u provider.Usage, ok bool) {
	switch v := payload.(type) {
	case provider.Usage:
		return v, true
	case *provider.Usage:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		in, iok := intFromAny(v["input_tokens"])
		out, ook := intFromAny(v["output_tokens"])
		cr, _ := intFromAny(v["cache_read_tokens"])
		cw, _ := intFromAny(v["cache_write_tokens"])
		return provider.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheWriteTokens: cw}, iok || ook
	}
	return provider.Usage{}, false
}

// stepEvent extracts a StepEvent from a KindStepFinish payload, tolerating both
// an agent.StepEvent value (the loop's direct-path emit) and a decoded JSON
// object (the broker/replay path, where the payload round-trips through storage
// with snake_case keys). Returns ok=false when the payload carries no step data.
func stepEvent(payload any) (se agent.StepEvent, ok bool) {
	switch v := payload.(type) {
	case agent.StepEvent:
		return v, true
	case *agent.StepEvent:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		reason, _ := v["finish_reason"].(string)
		if reason == "" {
			reason, _ = v["finishReason"].(string)
		}
		cont, _ := v["is_continued"].(bool)
		if _, present := v["isContinued"]; present {
			cont, _ = v["isContinued"].(bool)
		}
		se.FinishReason = reason
		se.IsContinued = cont
		if um, ok := v["usage"].(map[string]any); ok {
			in, _ := intFromAny(um["input_tokens"])
			if i, ok2 := intFromAny(um["inputTokens"]); ok2 {
				in = i
			}
			out, _ := intFromAny(um["output_tokens"])
			if o, ok2 := intFromAny(um["outputTokens"]); ok2 {
				out = o
			}
			se.Usage = &provider.Usage{InputTokens: in, OutputTokens: out}
		}
		return se, reason != ""
	}
	return agent.StepEvent{}, false
}

// interactionPayload extracts an Interaction from a KindInterrupt payload,
// tolerating an agent.Interaction value (the loop's direct-path emit — the
// serveChatDirect no-runtime path hands the struct itself) and a decoded JSON
// object (the broker/replay path, where the payload round-trips through storage
// with Go's default field-name keys). The lowercase aliases guard against a
// JSON round-trip that lowercases them. Returns ok=false when the payload
// carries no interaction data.
func interactionPayload(payload any) (in agent.Interaction, ok bool) {
	switch v := payload.(type) {
	case agent.Interaction:
		return v, true
	case *agent.Interaction:
		if v != nil {
			return *v, true
		}
	case map[string]any:
		id, _ := v["ID"].(string)
		if id == "" {
			id, _ = v["id"].(string)
		}
		kind, _ := v["Kind"].(string)
		if kind == "" {
			kind, _ = v["kind"].(string)
		}
		toolCallID, _ := v["ToolCallID"].(string)
		if toolCallID == "" {
			toolCallID, _ = v["toolCallID"].(string)
		}
		toolName, _ := v["ToolName"].(string)
		if toolName == "" {
			toolName, _ = v["toolName"].(string)
		}
		args, _ := v["Input"].(map[string]any)
		if args == nil {
			args, _ = v["input"].(map[string]any)
		}
		return agent.Interaction{ID: id, Kind: kind, ToolCallID: toolCallID, ToolName: toolName, Input: args}, true
	}
	return agent.Interaction{}, false
}

// intFromAny reads an int from a JSON-decoded numeric value (float64 by default,
// or json.Number when the decoder uses UseNumber).
func intFromAny(x any) (int, bool) {
	switch n := x.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
