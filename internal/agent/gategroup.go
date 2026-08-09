package agent

import (
	"context"

	"nowhere-agent/internal/toolruntime"
)

// GateGroup composes independent tool-authorization policies into a single
// GateFunc without the assembly point hand-nesting closures. It implements both
// GateFuncProvider and the Middleware marker, so it registers into a loop either
// directly (loop.Use(g)) or via PermissionMW:
//
//	g := agent.NewGateGroup()
//	g.Use(envPolicy)        // deny by env risk policy
//	g.Use(teamPolicy)       // deny by some future per-team policy
//	loop.Use(&agent.PermissionMW{Check: g.GateCheck()})
//
// Semantics are first-deny-wins: gates are consulted in registration order and
// the FIRST deny ends the consultation, returning that gate's reason (so the
// most specific/highest-priority policy registers first). When every gate
// allows, the call is allowed. The funcs are pure and cheap — the loop may
// consult the composed func twice per tool call (interaction gate then
// execution gate) — and resolve run-scoped inputs from the ctx at call time.
type GateGroup struct {
	gates []GateFunc
}

// NewGateGroup builds an empty gate group.
func NewGateGroup() *GateGroup { return &GateGroup{} }

// Use appends one policy to the group. First registered has highest priority
// (its deny wins). It returns the group for chaining.
func (g *GateGroup) Use(p GateFunc) *GateGroup {
	g.gates = append(g.gates, p)
	return g
}

// GateCheck supplies the composed policy to the loop.
func (g *GateGroup) GateCheck() GateFunc { return g.eval }

// MiddlewareName identifies the group in diagnostics.
func (g *GateGroup) MiddlewareName() string { return "gate-group" }

// eval is the first-deny-wins composition.
func (g *GateGroup) eval(ctx context.Context, t toolruntime.Tool) (bool, string) {
	for _, gate := range g.gates {
		if gate == nil {
			continue
		}
		if deny, reason := gate(ctx, t); deny {
			return true, reason
		}
	}
	return false, ""
}
