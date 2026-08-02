package dreaming

import (
	"sort"
	"time"

	"nowhere-agent/internal/memory"
)

// Caps bounds the LIVE memories of each kind in one scope
// (memory-consolidation). Deprecated memories do not count: they are already
// invisible to recall, and counting them would make the cap tighten as a side
// effect of ordinary supersession.
//
// Caps are per kind rather than one shared total because a shared total is won
// by whichever kind generates most freely. That is measured, not hypothetical:
// before caps existed, insights reached 83% of a live store (257 of 311) whose
// facts were the part with any value.
type Caps struct {
	// Facts bounds fact and preference together — both are "things true about
	// the user", and splitting them would force an arbitrary line between
	// "prefers X" and "is X".
	Facts     int
	Insights  int
	Summaries int
}

// DefaultCaps mirrors the config defaults, for callers that construct a Worker
// without a config (tests, and any embedder of the package).
func DefaultCaps() Caps { return Caps{Facts: 80, Insights: 30, Summaries: 40} }

// capGroup names one pool of the cap budget. Several kinds can share a pool.
type capGroup string

const (
	groupFacts     capGroup = "fact/preference"
	groupInsights  capGroup = "insight"
	groupSummaries capGroup = "summary"
)

// capGroupOf maps a kind to its pool. An unrecognized kind falls into the
// fact pool rather than into no pool at all: a kind added later must inherit
// *some* bound, because the failure mode this whole mechanism exists to prevent
// is exactly "a kind nothing bounds".
func capGroupOf(k memory.Kind) capGroup {
	switch k {
	case memory.KindInsight:
		return groupInsights
	case memory.KindSummary:
		return groupSummaries
	case memory.KindFact, memory.KindPreference:
		return groupFacts
	default:
		return groupFacts
	}
}

// limit returns the ceiling for a pool.
func (c Caps) limit(g capGroup) int {
	switch g {
	case groupInsights:
		return c.Insights
	case groupSummaries:
		return c.Summaries
	default:
		return c.Facts
	}
}

// forKind returns a kind's ceiling and whether it shares that ceiling with
// other kinds (which the prompt has to say out loud, or two groups counted
// against one budget read as two budgets).
func (c Caps) forKind(k memory.Kind) (int, bool) {
	g := capGroupOf(k)
	return c.limit(g), g == groupFacts
}

// countKind counts the handled memories that fall in a pool.
func countKind(existing []handled, g capGroup) int {
	n := 0
	for _, h := range existing {
		if capGroupOf(h.mem.Kind) == g {
			n++
		}
	}
	return n
}

// overCap returns the memories to evict from a pool: the oldest live ones, so
// that at most `limit` remain. Nil when the pool fits.
//
// This is the machine half of a two-stage enforcement. Consolidation is told
// each cap and asked to merge to fit, which preserves information that eviction
// would drop; this runs afterwards and does not care what it was asked. An
// invariant handed entirely to a model is not an invariant.
func overCap(live []memory.Memory, g capGroup, limit int) []memory.Memory {
	var pool []memory.Memory
	for _, m := range live {
		if !m.Deprecated && capGroupOf(m.Kind) == g {
			pool = append(pool, m)
		}
	}
	if len(pool) <= limit {
		return nil
	}
	// Oldest first; ties broken by id so eviction is deterministic when a batch
	// of memories shares a timestamp (which is the norm — one pass writes them
	// all within the same transaction).
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].CreatedAt.Equal(pool[j].CreatedAt) {
			return pool[i].ID < pool[j].ID
		}
		return pool[i].CreatedAt.Before(pool[j].CreatedAt)
	})
	return pool[:len(pool)-limit]
}

// liveOf filters a scope listing down to non-deprecated memories.
func liveOf(all []memory.Memory) []memory.Memory {
	var out []memory.Memory
	for _, m := range all {
		if !m.Deprecated {
			out = append(out, m)
		}
	}
	return out
}

// purgeCutoff is the instant before which deprecated memories are deleted.
func purgeCutoff(now time.Time, after time.Duration) time.Time {
	return now.Add(-after)
}
