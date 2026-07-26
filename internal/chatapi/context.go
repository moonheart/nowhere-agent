package chatapi

import (
	"context"
	"strings"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/memory"
	"nowhere-agent/internal/skill"
)

// ScopeResolver returns the scopes a user may read from (own user scope,
// their teams, system). *identity.Service satisfies it.
type ScopeResolver interface {
	AccessibleScopes(ctx context.Context, userID string) ([]identity.ScopeRef, error)
}

// contextBuilder is the default ContextBuilder: it renders the L0 skill index
// and composes it with the base system prompt. Memory recall is NOT part of the
// system prompt anymore — memories are surfaced incrementally into the outgoing
// message view (never the durable history) by the sessionMemoryInjector, so the
// system prompt stays byte-stable across requests and the LLM prompt prefix can
// be cached. (design D5; injection is capability K / context-mgmt)
type contextBuilder struct {
	base     string
	scopes   ScopeResolver
	memory   memory.Port
	skills   *skill.Engine
	// recallLimit caps how many memories are injected per request.
	recallLimit int
}

// NewContextBuilder composes skills + memory into the system prompt.
func NewContextBuilder(base string, scopes ScopeResolver, mem memory.Port, skills *skill.Engine) ContextBuilder {
	return &contextBuilder{base: base, scopes: scopes, memory: mem, skills: skills, recallLimit: 8}
}

// SystemPrompt builds: base + available skills (L0). Memory is deliberately
// excluded (see contextBuilder doc); the `query` param is accepted for interface
// compatibility but unused here.
func (c *contextBuilder) SystemPrompt(ctx context.Context, user identity.User, query string) string {
	scopes, err := c.scopes.AccessibleScopes(ctx, user.ID)
	if err != nil {
		scopes = []identity.ScopeRef{identity.UserScope(user.ID), identity.SystemScope()}
	}

	var sections []string
	if s := strings.TrimSpace(c.base); s != "" {
		sections = append(sections, s)
	}
	if c.skills != nil {
		if s := c.skills.RenderL0Prompt(ctx, scopes); s != "" {
			sections = append(sections, s)
		}
	}
	return strings.Join(sections, "\n\n")
}
