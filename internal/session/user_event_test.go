package session

import (
	"context"
	"testing"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/provider"
)

// TestSubmitRecordsUserEventBeforeRunOutput verifies the user turn is persisted
// at run start, so it deterministically precedes the run's terminal event in the
// durable log even when the run finishes very quickly.
func TestSubmitRecordsUserEventBeforeRunOutput(t *testing.T) {
	rt, rg, sess := newRegistrySession(t)
	userMsg := provider.TextMessage(provider.RoleUser, "hello there")
	run, err := rg.Submit(context.Background(), sess.ID, RunWork{
		Loop:        registryLoop(&stubProvider{deltas: []string{"hi"}}),
		UserMessage: &userMsg,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitSettle(t, rt, sess.ID)

	evs, err := rt.Replay(context.Background(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	userIdx, doneIdx := -1, -1
	for i, e := range evs {
		switch e.Kind {
		case string(agent.KindUser):
			userIdx = i
		case string(agent.KindDone):
			doneIdx = i
		}
	}
	if userIdx == -1 {
		t.Fatal("user event was not persisted")
	}
	if doneIdx == -1 {
		t.Fatal("done event was not persisted")
	}
	if userIdx > doneIdx {
		t.Errorf("user event (index %d) must precede the terminal event (index %d)", userIdx, doneIdx)
	}
}
