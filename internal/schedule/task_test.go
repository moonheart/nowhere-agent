package schedule

import (
	"testing"
	"time"
)

// nextFire drives a parsed schedule from a fixed reference, returning the next
// few fire instants so tests can assert exact local times across zones.
func nextFires(t *testing.T, task Task, from time.Time, n int) []time.Time {
	t.Helper()
	s, err := task.Schedule()
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	out := make([]time.Time, 0, n)
	cur := from
	for i := 0; i < n; i++ {
		cur = s.Next(cur)
		out = append(out, cur)
	}
	return out
}

func TestCronNextFire_Timezone(t *testing.T) {
	// "0 9 * * *" — 9am daily. The wall-clock hour must be 9 in the task's own
	// zone, which lands on different absolute instants per zone.
	from := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)

	ny := Task{Cron: "0 9 * * *", Timezone: "America/New_York"}
	fires := nextFires(t, ny, from, 1)
	loc, _ := time.LoadLocation("America/New_York")
	if got := fires[0].In(loc); got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("NY fire not at 9:00 local: %v", got)
	}

	sh := Task{Cron: "0 9 * * *", Timezone: "Asia/Shanghai"}
	firesSH := nextFires(t, sh, from, 1)
	shLoc, _ := time.LoadLocation("Asia/Shanghai")
	if got := firesSH[0].In(shLoc); got.Hour() != 9 {
		t.Fatalf("Shanghai fire not at 9:00 local: %v", got)
	}

	// Same wall-clock spec, different zones → different absolute instants.
	if fires[0].Equal(firesSH[0]) {
		t.Fatalf("NY and Shanghai 9am should differ in absolute time, both %v", fires[0])
	}
}

func TestCronNextFire_DefaultsUTC(t *testing.T) {
	task := Task{Cron: "30 14 * * *"} // no timezone
	fires := nextFires(t, task, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 1)
	if got := fires[0].UTC(); got.Hour() != 14 || got.Minute() != 30 {
		t.Fatalf("default zone should be UTC 14:30, got %v", got)
	}
}

func TestCronNextFire_DST(t *testing.T) {
	// America/New_York sprang forward on 2026-03-08. A daily 9am task must stay
	// at 9am local across the transition, not drift by an hour.
	loc, _ := time.LoadLocation("America/New_York")
	task := Task{Cron: "0 9 * * *", Timezone: "America/New_York"}
	from := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)
	for i, f := range nextFires(t, task, from, 3) {
		if got := f.In(loc); got.Hour() != 9 || got.Minute() != 0 {
			t.Fatalf("fire %d drifted off 9:00 local across DST: %v", i, got)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		task Task
		ok   bool
	}{
		{"free-text prompt", Task{Prompt: "daily summary", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject}, true},
		{"agent reference", Task{AgentDefName: "reviewer", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject}, true},
		{"neither source", Task{Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject}, false},
		{"bad cron", Task{Prompt: "x", Cron: "not a cron", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject}, false},
		{"bad timezone", Task{Prompt: "x", Cron: "0 9 * * *", Timezone: "Mars/Olympus", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject}, false},
		{"bad multitask", Task{Prompt: "x", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: "explode"}, false},
		{"bad on_run_completed", Task{Prompt: "x", Cron: "0 9 * * *", OnRunCompleted: "archive", Multitask: MultitaskReject}, false},
		{"valid webhook url", Task{Prompt: "x", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject, WebhookURL: "https://hooks.example.com/cb"}, true},
		{"bad webhook scheme", Task{Prompt: "x", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject, WebhookURL: "file:///etc/passwd"}, false},
		{"bare webhook host", Task{Prompt: "x", Cron: "0 9 * * *", OnRunCompleted: OnRunKeep, Multitask: MultitaskReject, WebhookURL: "hooks.example.com/cb"}, false},
	}
	for _, c := range cases {
		err := c.task.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: expected valid, got %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected invalid, got nil", c.name)
		}
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	endPassed := now.Add(-time.Hour)

	cases := []struct {
		name string
		task Task
		due  bool
	}{
		{"enabled and past next_run", Task{Enabled: true, NextRunAt: past}, true},
		{"disabled", Task{Enabled: false, NextRunAt: past}, false},
		{"not yet due", Task{Enabled: true, NextRunAt: future}, false},
		{"past end_time", Task{Enabled: true, NextRunAt: past, EndTime: &endPassed}, false},
	}
	for _, c := range cases {
		if got := c.task.Due(now); got != c.due {
			t.Errorf("%s: Due() = %v, want %v", c.name, got, c.due)
		}
	}
}

func TestPromptSource(t *testing.T) {
	if (Task{Prompt: "x"}).PromptSource() != SourcePrompt {
		t.Error("standalone prompt should be SourcePrompt")
	}
	if (Task{AgentDefName: "a", Prompt: "kick"}).PromptSource() != SourceAgentDef {
		t.Error("agent reference should be SourceAgentDef")
	}
}

func TestTextArrayRoundTrip(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{"read_file"},
		{"read_file", "write_file", "run_command"},
	}
	for _, c := range cases {
		got := parseTextArray(formatTextArray(c))
		if len(got) != len(c) {
			t.Errorf("round-trip %v -> %v", c, got)
			continue
		}
		for i := range c {
			if got[i] != c[i] {
				t.Errorf("round-trip %v -> %v", c, got)
				break
			}
		}
	}
}
