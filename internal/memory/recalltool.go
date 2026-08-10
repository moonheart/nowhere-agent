package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/toolruntime"
)

// RecallTool lets the agent ACTIVELY search long-term memory (capability K /
// context-mgmt). The auto-injection surfaces only preference/fact kinds; the
// model calls this to fetch the kinds NOT auto-injected — summary (compressed
// episode) and insight (cross-session pattern) — or to recall something older
// / more specific than the injected set. Read-only: it changes nothing.
type RecallTool struct {
	mem    Port
	scopes []identity.ScopeRef
}

// NewRecallTool creates a recall_memory tool resolving memories against the
// given scopes (mirrors the context builder's scope set).
func NewRecallTool(mem Port, scopes []identity.ScopeRef) *RecallTool {
	return &RecallTool{mem: mem, scopes: scopes}
}

func (t *RecallTool) Name() string { return "recall_memory" }

func (t *RecallTool) Description() string {
	return "Search long-term memory about the user/team. Auto-injected context only covers " +
		"preference/fact; call this to fetch summaries of past conversations, higher-level " +
		"insights, or any older/specific memory not in the injected set. Pass a query describing " +
		"what to recall. Read-only."
}

func (t *RecallTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "what to recall, ranked by relevance"},
			"kinds": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string", "enum": []string{"fact", "preference", "insight", "summary"}},
				"description": "optional; defaults to [summary, insight] (the kinds NOT auto-injected)",
			},
			"limit": map[string]any{"type": "integer", "description": "optional, default 8, max 20"},
		},
		"required": []string{"query"},
	}
}

// Risk is read-only: recalling memory returns text and changes nothing.
func (t *RecallTool) Risk() toolruntime.Risk { return toolruntime.RiskReadOnly }

func (t *RecallTool) Timeout() time.Duration { return 15 * time.Second }

// Call recalls memories matching the query (full relevance recall — zero time
// lower bound), filtered to the requested kinds. An empty result is a friendly
// non-error so the model can rephrase or conclude there is nothing to recall.
func (t *RecallTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	query, _ := args["query"].(string)
	if strings.TrimSpace(query) == "" {
		return toolruntime.Result{Content: `missing required argument "query"`, IsError: true}, nil
	}

	kinds := []Kind{KindSummary, KindInsight} // the kinds NOT auto-injected
	if raw, ok := args["kinds"].([]any); ok && len(raw) > 0 {
		kinds = kinds[:0]
		for _, v := range raw {
			if s, ok := v.(string); ok {
				kinds = append(kinds, Kind(s))
			}
		}
	}
	limit := 8
	if f, ok := args["limit"].(float64); ok && f > 0 {
		limit = int(f)
		if limit > 20 {
			limit = 20
		}
	}

	mems, err := t.mem.RecallSince(ctx, time.Time{}, query, t.scopes, kinds, limit)
	if err != nil {
		return toolruntime.Result{}, err
	}
	if len(mems) == 0 {
		return toolruntime.Result{Content: fmt.Sprintf("no memories found for query %q (kinds %v)", query, kinds)}, nil
	}
	var b strings.Builder
	for _, m := range mems {
		// The trailing id lets the model reference the memory with the
		// edit_memory / forget_memory tools.
		b.WriteString(fmt.Sprintf("- [%s] (%s) %s [id: %s]\n", m.CreatedAt.Format("2006-01-02"), m.Kind, m.Content, m.ID))
	}
	return toolruntime.Result{Content: b.String()}, nil
}
