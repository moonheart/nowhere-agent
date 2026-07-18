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
// and recalls long-term memories relevant to the query, composing them with
// the base system prompt. Recall is read-side only; memory is written solely
// by the dreaming worker (design D5).
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

// SystemPrompt builds: base + available skills (L0) + relevant memories.
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
	if c.memory != nil && strings.TrimSpace(query) != "" {
		if s := c.recallSection(ctx, query, scopes); s != "" {
			sections = append(sections, s)
		}
	}
	return strings.Join(sections, "\n\n")
}

// recallSection formats recalled memories as a system-prompt section.
func (c *contextBuilder) recallSection(ctx context.Context, query string, scopes []identity.ScopeRef) string {
	mems, err := c.memory.Recall(ctx, query, scopes, c.recallLimit)
	if err != nil || len(mems) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant memories about the user/team:\n")
	for _, m := range mems {
		b.WriteString("- ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}
