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
	// The other streaming-content kinds stay on the broker.
	for _, k := range []agent.EventKind{
		agent.KindText, agent.KindThinking, agent.KindToolUse, agent.KindToolResult, agent.KindSubagent,
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
