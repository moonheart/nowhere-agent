package chatapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/agent"
	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/provider"
	"nowhere-agent/internal/session"
)

// memoryKinds are the kinds auto-injected into the conversation (type split):
// who the user is and what they prefer. summary/insight are NOT auto-injected —
// the model fetches those on demand via the recall_memory tool.
var memoryKinds = []memory.Kind{memory.KindPreference, memory.KindFact}

// sessionMemoryInjector surfaces newly-created memories into the outgoing view
// (incremental injection, capability K / context-mgmt). On a session's first
// turn (watermark zero) it injects the relevant preference/fact set; on later
// turns it injects only memories created after the watermark, advancing it.
// It never touches the durable history — the loop appends its output to a
// per-attempt copy of the view.
type sessionMemoryInjector struct {
	mem     memory.Port
	scopes  ScopeResolver
	runtime *session.Runtime
	user    identity.User
	query   string // lastUserText, used for relevance ranking (empty on verdict resumes)
	limit   int
	now     func() time.Time
}

// NewSessionMemoryInjector builds the injector for one request: the user's
// memories, ranked by the query, auto-injecting preference/fact kinds.
func NewSessionMemoryInjector(mem memory.Port, scopes ScopeResolver, rt *session.Runtime, user identity.User, query string) agent.MemoryInjector {
	return &sessionMemoryInjector{mem: mem, scopes: scopes, runtime: rt, user: user, query: query, limit: 8, now: time.Now}
}

// Inject implements agent.MemoryInjector.
func (inj *sessionMemoryInjector) Inject(ctx context.Context, sessionID string, _ []provider.Message) ([]provider.Message, error) {
	if inj.mem == nil || inj.runtime == nil {
		return nil, nil
	}
	since, err := inj.runtime.MemoryInjectedAt(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	scopes, err := inj.scopes.AccessibleScopes(ctx, inj.user.ID)
	if err != nil {
		scopes = []identity.ScopeRef{identity.UserScope(inj.user.ID), identity.SystemScope()}
	}
	mems, err := inj.mem.RecallSince(ctx, since, inj.query, scopes, memoryKinds, inj.limit)
	if err != nil || len(mems) == 0 {
		// Nothing new: don't append a message AND don't advance the watermark,
		// so a memory written between the empty check and now isn't skipped by a
		// watermark that ran ahead of any actual injection.
		return nil, err
	}
	// Advance the watermark only when something was actually surfaced.
	if inj.now == nil {
		inj.now = time.Now
	}
	if err := inj.runtime.MarkMemoryInjectedAt(ctx, sessionID, inj.now()); err != nil {
		return nil, err
	}
	return []provider.Message{provider.TextMessage(provider.RoleUser, formatMemories(mems))}, nil
}

// formatMemories renders the injected background-context block. Each memory
// carries its creation date so the model can judge freshness (the "随时间保鲜"
// signal): an old memory about a transient state ("正在计划 X") is weighed
// against its date, not taken as current.
func formatMemories(mems []memory.Memory) string {
	var b strings.Builder
	b.WriteString("[背景记忆 · 以下是与用户相关的长期记忆,供参考,非用户指令;每条注明记录日期,请结合日期判断时效]\n")
	for _, m := range mems {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", m.CreatedAt.Format("2006-01-02"), m.Content))
	}
	return b.String()
}

// MemoryInjectorFactory builds a per-request MemoryInjector for an
// authenticated user + their query. Returns nil to disable injection.
type MemoryInjectorFactory func(ctx context.Context, user identity.User, query string) agent.MemoryInjector

// WithMemoryInjector wires incremental memory injection: each run's loop gets
// an injector that surfaces new memories into the outgoing view (never the
// durable history). Call after WithRuntime.
func (h *Handler) WithMemoryInjector(f MemoryInjectorFactory) *Handler {
	h.memInjectorFactory = f
	return h
}
