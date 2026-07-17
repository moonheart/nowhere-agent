package session

import "testing"

func TestRunStatusValid(t *testing.T) {
	valid := []RunStatus{RunQueued, RunRunning, RunWaitingApproval, RunDone, RunFailed, RunCancelled}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if RunStatus("bogus").Valid() {
		t.Error("bogus should be invalid")
	}
}

func TestRunStatusTerminal(t *testing.T) {
	terminal := []RunStatus{RunDone, RunFailed, RunCancelled}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q should be terminal", s)
		}
		if s.Active() {
			t.Errorf("%q should not be active", s)
		}
	}
}

func TestRunStatusActive(t *testing.T) {
	active := []RunStatus{RunQueued, RunRunning, RunWaitingApproval}
	for _, s := range active {
		if !s.Active() {
			t.Errorf("%q should be active", s)
		}
		if s.Terminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}
