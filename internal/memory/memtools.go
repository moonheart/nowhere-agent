package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nowhere-agent/internal/identity"
	"nowhere-agent/internal/toolruntime"
)

// The agent's ACTIVE memory-maintenance tools: write_memory stores a fact or
// preference on the caller's behalf, edit_memory rewrites one by id, and
// forget_memory deletes one. Together they let the agent maintain what it
// knows about the user beyond what the dreaming worker consolidates offline.
//
// All three are pinned to the caller's USER scope: the tools are constructed
// with the session owner's user id (resolved at registration, like
// recall_memory's scopes), never accept a scope from the model, and
// edit/forget verify the target memory's scope before touching it.

// authorizeUserMemory looks up a memory and verifies it belongs to the tool's
// user, mirroring adminapi's authorizeMemoryScope pattern: a wrong scope and
// a missing id report identically, so a caller holding a valid uuid cannot
// probe whether someone else's memory exists. It returns the memory on
// success, or a friendly is_error result.
func authorizeUserMemory(ctx context.Context, mem Port, id, userID string) (Memory, toolruntime.Result) {
	m, err := mem.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Memory{}, toolruntime.Result{
				Content: fmt.Sprintf("no memory with id %q (memories come from recall_memory)", id),
				IsError: true,
			}
		}
		return Memory{}, toolruntime.Result{
			Content: fmt.Sprintf("look up memory %q: %v", id, err),
			IsError: true,
		}
	}
	if m.Scope.Scope != identity.ScopeUser || m.Scope.UserID != userID {
		return Memory{}, toolruntime.Result{
			Content: fmt.Sprintf("no memory with id %q (memories come from recall_memory)", id),
			IsError: true,
		}
	}
	return m, toolruntime.Result{}
}

// validKind reports whether s is one of the four memory kinds.
func validKind(s string) bool {
	switch Kind(s) {
	case KindFact, KindPreference, KindInsight, KindSummary:
		return true
	}
	return false
}

/* ---------- write_memory ---------- */

// WriteMemoryTool stores a new user-scope memory on the caller's behalf.
type WriteMemoryTool struct {
	mem    Port
	userID string
}

// NewWriteMemoryTool creates the write_memory tool pinned to one user's scope.
func NewWriteMemoryTool(mem Port, userID string) *WriteMemoryTool {
	return &WriteMemoryTool{mem: mem, userID: userID}
}

func (t *WriteMemoryTool) Name() string { return "write_memory" }

func (t *WriteMemoryTool) Description() string {
	return "Write something the user told you into their long-term memory, so future conversations " +
		"remember it. kind: fact (a durable fact about the user), preference (what the user likes or " +
		"wants), insight (a pattern you noticed), summary (a condensed episode). The memory becomes " +
		"recallable immediately and is scoped to the user. Prefer asking the user before storing anything " +
		"sensitive."
}

func (t *WriteMemoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{"fact", "preference", "insight", "summary"},
				"description": "what kind of memory this is",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "the memory itself, as a complete self-contained sentence",
			},
		},
		"required": []string{"kind", "content"},
	}
}

// Risk is external-write: it mutates the user's durable long-term memory,
// which shapes every future conversation, so the permission gate reviews it.
func (t *WriteMemoryTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }

func (t *WriteMemoryTool) Timeout() time.Duration { return 15 * time.Second }

func (t *WriteMemoryTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	kind, _ := args["kind"].(string)
	content, _ := args["content"].(string)
	if !validKind(kind) {
		return toolruntime.Result{
			Content: `"kind" must be one of: fact, preference, insight, summary`,
			IsError: true,
		}, nil
	}
	if strings.TrimSpace(content) == "" {
		return toolruntime.Result{Content: `"content" must be a non-empty string`, IsError: true}, nil
	}
	m, err := t.mem.Store(ctx, Memory{
		Scope:   identity.UserScope(t.userID),
		Kind:    Kind(kind),
		Content: content,
	})
	if err != nil {
		return toolruntime.Result{}, err
	}
	return toolruntime.Result{Content: fmt.Sprintf("saved memory %s (%s): %s", m.ID, m.Kind, m.Content)}, nil
}

/* ---------- edit_memory ---------- */

// EditMemoryTool rewrites one of the caller's memories in place (content only;
// kind and scope are immutable). Callers get the id from recall_memory.
type EditMemoryTool struct {
	mem    Port
	userID string
}

// NewEditMemoryTool creates the edit_memory tool pinned to one user's scope.
func NewEditMemoryTool(mem Port, userID string) *EditMemoryTool {
	return &EditMemoryTool{mem: mem, userID: userID}
}

func (t *EditMemoryTool) Name() string { return "edit_memory" }

func (t *EditMemoryTool) Description() string {
	return "Rewrite the content of a memory you know by id (ids come from recall_memory). " +
		"Only the content changes — the kind and the user scope are fixed. Only your own memories are editable."
}

func (t *EditMemoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "the memory's id from recall_memory"},
			"content": map[string]any{
				"type":        "string",
				"description": "the corrected memory content, as a complete self-contained sentence",
			},
		},
		"required": []string{"id", "content"},
	}
}

func (t *EditMemoryTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }

func (t *EditMemoryTool) Timeout() time.Duration { return 15 * time.Second }

func (t *EditMemoryTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	id, _ := args["id"].(string)
	content, _ := args["content"].(string)
	if strings.TrimSpace(id) == "" {
		return toolruntime.Result{Content: `missing required argument "id"`, IsError: true}, nil
	}
	if strings.TrimSpace(content) == "" {
		return toolruntime.Result{Content: `"content" must be a non-empty string`, IsError: true}, nil
	}
	if _, denied := authorizeUserMemory(ctx, t.mem, id, t.userID); denied.IsError {
		return denied, nil
	}
	if err := t.mem.Update(ctx, id, content); err != nil {
		return toolruntime.Result{}, err
	}
	return toolruntime.Result{Content: fmt.Sprintf("updated memory %s: %s", id, content)}, nil
}

/* ---------- forget_memory ---------- */

// ForgetMemoryTool permanently deletes one of the caller's memories by id.
type ForgetMemoryTool struct {
	mem    Port
	userID string
}

// NewForgetMemoryTool creates the forget_memory tool pinned to one user's scope.
func NewForgetMemoryTool(mem Port, userID string) *ForgetMemoryTool {
	return &ForgetMemoryTool{mem: mem, userID: userID}
}

func (t *ForgetMemoryTool) Name() string { return "forget_memory" }

func (t *ForgetMemoryTool) Description() string {
	return "Permanently delete a memory you know by id (ids come from recall_memory). " +
		"The user asked for it to be forgotten, or the memory is wrong. Only your own memories can be deleted."
}

func (t *ForgetMemoryTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "the memory's id from recall_memory"},
		},
		"required": []string{"id"},
	}
}

func (t *ForgetMemoryTool) Risk() toolruntime.Risk { return toolruntime.RiskExternalWrite }

func (t *ForgetMemoryTool) Timeout() time.Duration { return 15 * time.Second }

func (t *ForgetMemoryTool) Call(ctx context.Context, args map[string]any) (toolruntime.Result, error) {
	id, _ := args["id"].(string)
	if strings.TrimSpace(id) == "" {
		return toolruntime.Result{Content: `missing required argument "id"`, IsError: true}, nil
	}
	if _, denied := authorizeUserMemory(ctx, t.mem, id, t.userID); denied.IsError {
		return denied, nil
	}
	if err := t.mem.Forget(ctx, id); err != nil {
		return toolruntime.Result{}, err
	}
	return toolruntime.Result{Content: fmt.Sprintf("forgot memory %s", id)}, nil
}
