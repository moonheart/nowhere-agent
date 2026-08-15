package main

import (
	"context"
	"time"

	"nowhere-agent/internal/quota"
	"nowhere-agent/internal/usage"
)

// wire_usage.go — billing attribution and the budget gate. Extracted verbatim
// from run() (see deps.go).

func (d *serverDeps) wireUsage() {
	// Billing attribution (enterprise-readiness P1-3): a run is stamped with the
	// team whose provider assignment pays for it, so per-team cost reports read
	// the run row directly. Attribution mirrors resolution: the team is billed
	// only when its own assignment actually serves the request; anything else is
	// platform-billed. A hiccup yields "" (platform-billed), never a blocked run.
	// Shared by the chat handler and the scheduled-task trigger, which attribute
	// the same way as a human run.
	d.teamAttributor = func(ctx context.Context, userID string) string {
		teamID, err := d.provStore.UserTeam(ctx, userID)
		if err != nil || teamID == "" {
			return ""
		}
		a, err := d.provStore.GetTeamAssignment(ctx, teamID)
		if err != nil {
			return ""
		}
		t, err := d.provResolver.ResolveForTeam(ctx, teamID)
		if err != nil || t.ProviderID != a.ProviderID {
			return ""
		}
		return teamID
	}

	// Budget enforcement (enterprise-readiness P1-1): the platform records token
	// usage; this is what makes a monthly limit bite. A quota.Checker compares the
	// caller's (and billing team's) current-month billable tokens against the rows
	// in usage_budgets and rejects at submit, before any model spend. Spend lookups
	// are thin adapters over the usage store (billable = input+output, the pair
	// providers price). Fail-open inside the checker: a usage/budget DB hiccup
	// never blocks a run. Shared by the chat handler and the scheduled-task trigger.
	d.usageStore = usage.NewStore(d.pool)
	d.budgetChecker = quota.NewChecker(quota.NewStore(d.pool),
		func(ctx context.Context, userID string, from, to time.Time) (int64, error) {
			t, err := d.usageStore.ForUser(ctx, userID, usage.Range{From: from, To: to})
			return t.Total(), err
		},
		func(ctx context.Context, teamID string, from, to time.Time) (int64, error) {
			t, err := d.usageStore.ForTeam(ctx, teamID, usage.Range{From: from, To: to})
			return t.Total(), err
		})
}
