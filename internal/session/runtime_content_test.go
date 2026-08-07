package session

import (
	"testing"

	"nowhere-agent/internal/agent"
)

// TestIsContentKindRoutesInterruptToBroker pins the routing a prior rename broke.
// The interrupt frame (agent.KindInterrupt) MUST classify as a content kind so
// AppendEvent publishes it to the live broker, where the attach path translates
// it into the data-interaction frame that drives the client's approval /
// ask_user / client-tool card. When the kind's value was renamed from
// "approval_request" to "interrupt", this classifier kept checking the stale
// string, so every interaction frame was silently dropped in the runtime-wired
// server and a suspended run "just ended" with no visible prompt.
func TestIsContentKindRoutesInterruptToBroker(t *testing.T) {
	if !isContentKind(string(agent.KindInterrupt)) {
		t.Fatalf("agent.KindInterrupt (%q) must be a content kind so the data-interaction frame reaches the broker", agent.KindInterrupt)
	}
	// The other streaming-content kinds stay on the broker. KindToolArgs (the
	// incremental argument stream) is ephemeral like text: it must reach the
	// broker so a large tool input renders live, but is never persisted.
	// KindUsage is broker-routed too: it renders the live data-usage frame and
	// the finish frame's counts. When it was NOT a content kind it fell to the
	// lifecycle path (whose handler drops it), so live streams showed usage:0
	// and only a history reload surfaced the real counts.
	for _, k := range []agent.EventKind{
		agent.KindText, agent.KindThinking, agent.KindToolUse, agent.KindToolArgs, agent.KindToolResult, agent.KindSubagent, agent.KindUsage,
		// Step frames are live render detail (start-step/finish-step), served for
		// settled runs via /history, so they ride the broker and are never persisted.
		agent.KindStepStart, agent.KindStepFinish,
	} {
		if !isContentKind(string(k)) {
			t.Errorf("%q should be a content kind", k)
		}
	}
	// Lifecycle kinds stay on the durable/bus path.
	for _, k := range []agent.EventKind{
		agent.KindRunning, agent.KindDone, agent.KindError, agent.KindCancelled, agent.KindUser,
	} {
		if isContentKind(string(k)) {
			t.Errorf("%q should be a lifecycle kind, not content", k)
		}
	}
}
