package chatapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/session"
)

// attach.go — the attach state machine every chat client traverses
// (submitter and re-attacher alike): subscribe-first, replay/catch-up,
// live-follow with fillGap recovery, and the settle-detection trio
// (terminal lifecycle event, one-shot ActiveRun check, silent-fallback
// poll). The protocol frames it writes live in emitter.go; the resume
// entrypoint and its frame helpers live in resume.go.

// settlePollSilence is how long the attach loop tolerates frame silence before
// its settle poll falls back to the ActiveRun check (see the poll in attach).
// While a run's frames flow the run is provably active, so the DB check is
// skipped; the window is the cost of noticing a settle whose terminal event
// was lost.
const settlePollSilence = 5 * time.Second

// settleCheckInitial / settleCheckMax bound the backoff between consecutive
// ActiveRun fallback checks once the attach loop has entered the silence
// fallback. The check is a DB query for a multi-instance attach (memory
// runtime miss → PGStore.ActiveRun), so checking on every 250ms poll tick
// would hit the DB at 4 qps per attached client for the whole duration of a
// silent run (a 120s run_command emits no frames); backing off 1s → 5s caps
// that at one query per 5s per client in steady state, at the price of
// noticing a dropped-terminal-event settle up to 5s later.
const (
	settleCheckInitial = 1 * time.Second
	settleCheckMax     = 5 * time.Second
)

// attach streams a run's output to the client over the live StreamBroker (no
// database on the path). It subscribes first (so no live frame falls into the
// subscribe/catch-up gap), replays the retained buffer from `after`, then
// live-follows until the run settles or the client disconnects. Content frames
// come from the broker; the run's settled state comes from the runtime (the
// durable lifecycle log). Shared by serveChat (submitter, after=0) and
// serveResume (attacher): every client traverses this one path.
//
// pre is an optional set of frames written right after the `start` frame (e.g.
// the submitter's data-session frame).
func (h *Handler) attach(w http.ResponseWriter, r *http.Request, sessionID string, run session.Run, after int64, pre []chunk) {
	flusher := w.(http.Flusher)
	emitter := newSSEEmitter(w, flusher, uuid.NewString(), "text-1", "reasoning-1")
	emitter.write(chunk{"type": "start", "messageId": emitter.msgID})
	for _, c := range pre {
		emitter.write(c)
	}

	// A settled run has no live stream: its content is durable in the message
	// store and delivered to the client via serveHistory, so there is nothing to
	// attach to here. Just close the message cleanly — with the run's real terminal
	// reason (a failed run must not finish "stop").
	if run.Status.Terminal() {
		h.settleFinish(r, emitter, sessionID, run.ID, run.Status)
		return
	}

	broker := h.runtime.Broker()

	// Subscribe to BOTH channels before any catch-up, so nothing published during
	// the catch-up below is lost: content deltas from the broker (no DB on the
	// path) and lifecycle events from the bus (running/cancelled — the latter
	// terminates this stream even when no further content frame arrives).
	contentCh, unsubContent := broker.Subscribe(sessionID, 256)
	defer unsubContent()
	lifecycleCh, unsubLifecycle := h.runtime.Subscribe(sessionID, 16)
	defer unsubLifecycle()

	// Replay the run's durable lifecycle events (running) so a client that
	// attached after they were published still learns the run started. Lifecycle
	// is low-volume, so a durable replay here is cheap and off the hot path.
	if lifecycle, err := h.runtime.Replay(r.Context(), run.ID, 0); err == nil {
		for _, e := range lifecycle {
			if agent.EventKind(e.Kind) == agent.KindUser {
				continue // the client already has its own user message
			}
			emitLifecycleEvent(r, emitter, e)
		}
	}

	// Catch up on content frames retained in the broker that this client hasn't seen.
	maxOffset := after
	if retained, err := broker.Read(r.Context(), sessionID, after); err == nil {
		for _, e := range retained {
			if !runScoped(e.RunID, run.ID) || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		}
	}

	// Live-follow until the run settles or the client disconnects. Settle
	// detection is event-driven: the run's terminal lifecycle event
	// (done/error/cancelled) is the primary signal, and a one-shot ActiveRun
	// check right after subscribe covers a run that settled between the
	// caller's pre-check and this loop.
	settlePoll := time.NewTicker(250 * time.Millisecond)
	defer settlePoll.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	// One-shot settle check: a run that settled since the caller's pre-check
	// will never send another frame, and catching it here is free (this is the
	// case the first poll tick used to pay for).
	if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); !stillActive {
		maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
		h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
		h.settleFinish(r, emitter, sessionID, run.ID, "")
		return
	}

	// terminal latches once this run's terminal lifecycle event is observed.
	// From then on the poll tick closes the stream without another DB check;
	// the tick is the trailing-content grace (content may still be in the
	// broker poller's pipeline — the terminal event and the last content frame
	// travel different channels).
	terminal := false
	// lastFrame marks when this attacher last saw a frame of ITS run (content
	// or lifecycle). While frames flow the run is provably active, so the poll
	// skips its ActiveRun check entirely; it only falls back to the DB after
	// settlePollSilence of silence — a run that settled without reporting a
	// terminal event (a dropped bus event, or a run force-settled without one)
	// must be noticed somehow. Once in the fallback, consecutive checks back
	// off settleCheckBackoff (1s → 5s cap): a multi-instance attach (memory
	// runtime miss → PGStore.ActiveRun per check) costs one DB query per
	// backoff interval per client while following a silent-but-active run,
	// instead of one per 250ms poll tick. The one-shot check above still
	// bounds the settle latency at attach time, so the fallback only stretches
	// the pathological dropped-terminal-event case.
	lastFrame := time.Now()
	settleCheckBackoff := settleCheckInitial
	lastSettleCheck := time.Now()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// Heartbeat: only while the run is silent do idle-cutoff proxies
			// threaten the connection; when frames flow the stream is already
			// alive, so skip the comment frame. terminal runs close on the
			// poll tick and need no heartbeat.
			if !terminal && time.Since(lastFrame) >= heartbeatInterval {
				emitter.ping()
			}
		case <-settlePoll.C:
			// While the run's frames are flowing (or were very recent) the run
			// is provably active: skip the ActiveRun check and keep following.
			// Only after settlePollSilence of silence does the poll fall back
			// to the DB — the fallback exists for settles whose terminal event
			// was lost, and skipping it while frames flow is what keeps a
			// multi-instance attach off the DB while an active run streams.
			if !terminal && time.Since(lastFrame) <= settlePollSilence {
				continue
			}
			if !terminal {
				// Throttle the fallback itself: without the backoff this
				// branch runs on every 250ms tick for as long as a silent run
				// stays active.
				if time.Since(lastSettleCheck) < settleCheckBackoff {
					continue
				}
				lastSettleCheck = time.Now()
				if _, stillActive, _ := h.runtime.ActiveRun(r.Context(), sessionID); stillActive {
					settleCheckBackoff *= 2
					if settleCheckBackoff > settleCheckMax {
						settleCheckBackoff = settleCheckMax
					}
					continue
				}
			}
			maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
			h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
			h.settleFinish(r, emitter, sessionID, run.ID, "")
			return
		case e, open := <-lifecycleCh:
			if !open {
				continue
			}
			if e.RunID != run.ID {
				continue
			}
			emitLifecycleEvent(r, emitter, e)
			lastFrame = time.Now()
			settleCheckBackoff = settleCheckInitial // frames flowing: next silence restarts the backoff
			if terminalLifecycle(e.Kind) {
				terminal = true
			}
		case e, open := <-contentCh:
			if !open {
				maxOffset = h.drainContent(r, emitter, broker, sessionID, contentCh, run.ID, maxOffset)
				h.drainLifecycle(r, emitter, lifecycleCh, run.ID)
				h.settleFinish(r, emitter, sessionID, run.ID, "")
				return
			}
			if !runScoped(e.RunID, run.ID) || e.Offset <= maxOffset {
				continue
			}
			// A slow consumer's live frames get dropped by the broker (mem
			// broker and Redis poller alike) — recoverable via Read. Detect the
			// hole on the next delivered frame and fill it BEFORE emitting, so
			// the stream the client renders has no silent gaps. The gap frames
			// are emitted in offset order (Read is oldest-first) and bounded
			// strictly below `e`; anything at or above it is delivered live
			// next or caught by a later hole check.
			if e.Offset > maxOffset+1 {
				maxOffset = h.fillGap(r, emitter, broker, sessionID, run.ID, maxOffset, e.Offset)
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
			lastFrame = time.Now()
			settleCheckBackoff = settleCheckInitial // frames flowing: next silence restarts the backoff
		}
	}
}

// terminalLifecycle reports whether a lifecycle event ends its run. The run's
// terminal frame (done/error/cancelled) is the attach loop's primary settle
// signal; the settle poll exists only for settles it never reported.
func terminalLifecycle(kind string) bool {
	switch agent.EventKind(kind) {
	case agent.KindDone, agent.KindError, agent.KindCancelled:
		return true
	default:
		return false
	}
}

// fillGap recovers live frames dropped for this consumer between maxOffset and
// next (the offset of the live frame just received): the broker retained them
// in its ring (Read returns everything after maxOffset), so re-read and emit
// them in offset order. Frames at or above `next` are left to the live channel
// — they arrive in publish order after `e`, or a later hole check catches them
// if they too are dropped. It returns the new max offset, which the caller
// advances past `next` next. The broker Read is non-blocking (mem ring under a
// mutex; Redis XREAD with no block) and runs on the attach's own goroutine, so
// it cannot deadlock the publish path or interleave with the select loop.
func (h *Handler) fillGap(r *http.Request, emitter *sseEmitter, broker session.StreamBroker, sessionID, runID string, maxOffset, next int64) int64 {
	gap, err := broker.Read(r.Context(), sessionID, maxOffset)
	if err != nil {
		// A read failure leaves the hole unfilled (the run's content is still
		// durable in the message store; a reload repairs the view).
		return maxOffset
	}
	for _, ge := range gap {
		if !runScoped(ge.RunID, runID) || ge.Offset <= maxOffset || ge.Offset >= next {
			continue
		}
		maxOffset = ge.Offset
		emitStreamEvent(r, emitter, ge)
	}
	return maxOffset
}

// settleFinish terminates an attached stream with the correct terminal finish
// reason. The latched reason (set when this emitter saw the terminal
// KindError/KindCancelled) is the common path — the run's terminal lifecycle
// event is persisted and published before the run settles, so it almost always
// arrived first. When it didn't (a late attacher, or the settle-poll firing
// before the buffered event drained), re-fetch the run's terminal status and
// map it: a failed run must not finish "stop" (which would show a cut-off answer
// as a clean completion). statusOverride, when terminal, is used directly and
// skips the re-fetch (the settled-run early-return already knows the status).
func (h *Handler) settleFinish(r *http.Request, e *sseEmitter, sessionID, runID string, statusOverride session.RunStatus) {
	if e.finishReason != "" {
		e.finish()
		return
	}
	status := statusOverride
	if !status.Terminal() && h.runtime != nil {
		if runs, err := h.runtime.RunsForSession(r.Context(), sessionID); err == nil {
			for _, run := range runs {
				if run.ID == runID {
					status = run.Status
					break
				}
			}
		}
	}
	reason := "stop"
	switch status {
	case session.RunFailed:
		reason = "error"
	case session.RunCancelled:
		reason = "other"
	}
	e.finishWithReason(reason)
}

// drainLifecycle flushes lifecycle events still buffered on the subscription
// before the stream is settled. The terminal KindError/KindCancelled rides the
// lifecycle bus (not the content broker), so an attacher that observes the
// settle first — via the poll or right after a content frame — must drain this
// channel too, or the run's terminal event is stranded unread: the stream then
// ends with a status-mapped finish and NO error frame. Non-blocking, like
// drainContent.
func (h *Handler) drainLifecycle(r *http.Request, emitter *sseEmitter, lifecycleCh <-chan session.Event, runID string) {
	for {
		select {
		case e, open := <-lifecycleCh:
			if !open {
				return
			}
			if e.RunID != runID {
				continue
			}
			emitLifecycleEvent(r, emitter, e)
		default:
			return
		}
	}
}

// runScoped reports whether a content frame belongs to the stream of the run
// being attached. A frame with an EMPTY RunID is session-scoped — an
// out-of-band session_state write (Runtime.SetSessionStateKV with no active
// run, e.g. the client state endpoint between runs) — and every attached
// client of the session accepts it, so the plan panel stays live across runs.
func runScoped(eRunID, runID string) bool {
	return eRunID == runID || eRunID == ""
}

// drainContent flushes any content frames still buffered on the subscription
// before the stream is settled. It closes the race where the run completes and
// its terminal lifecycle fires before the client has drained the broker backlog
// (the run finishing schedules the retained frames for cleanup via Settle, so
// Read alone can't be relied on once it runs): without this drain, fast runs —
// notably the step frames of a multi-iteration tool run — would be dropped
// between the last frame the client saw and the finish. Non-blocking: only
// frames already queued are taken. After the channel drain, a final broker Read
// recovers frames dropped for this slow consumer that are still retained in the
// ring — the run-map removal (which settle detection observes) precedes
// broker.Settle's ring clear, so there is a window where the ring still holds
// them.
func (h *Handler) drainContent(r *http.Request, emitter *sseEmitter, broker session.StreamBroker, sessionID string, contentCh <-chan session.StreamEvent, runID string, maxOffset int64) int64 {
	for {
		select {
		case e, open := <-contentCh:
			if !open {
				return maxOffset
			}
			if !runScoped(e.RunID, runID) || e.Offset <= maxOffset {
				continue
			}
			maxOffset = e.Offset
			emitStreamEvent(r, emitter, e)
		default:
			// Channel drained. Recover frames the broker dropped for this slow
			// consumer while they are still retained: the run-map removal the
			// settle detection observed precedes broker.Settle's ring clear, so
			// Read can still see them in this window.
			if retained, err := broker.Read(r.Context(), sessionID, maxOffset); err == nil {
				for _, ge := range retained {
					if !runScoped(ge.RunID, runID) || ge.Offset <= maxOffset {
						continue
					}
					maxOffset = ge.Offset
					emitStreamEvent(r, emitter, ge)
				}
			}
			return maxOffset
		}
	}
}
