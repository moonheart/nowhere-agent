package toolruntime

// Names returns the names of all registered tools (unordered).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for n := range r.tools {
		out = append(out, n)
	}
	return out
}

// Scoped returns a new Registry holding a filtered view of r's tools, for a
// child run. The parent registry is never mutated.
//
//   - allow nil or containing "*" includes every tool; otherwise only the
//     named tools that exist in r are included.
//   - deny removes tools by name after allow resolution.
//   - exclude removes tools by name unconditionally (used to withhold the
//     spawn tool at the maximum subagent depth).
//
// The view shares the same Tool instances as the parent, so session-bound
// tools (e.g. the sandbox-bound file tools) keep operating on the same session.
func (r *Registry) Scoped(allow, deny []string, exclude ...string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	wildcard := len(allow) == 0
	allowSet := make(map[string]bool, len(allow))
	for _, n := range allow {
		if n == "*" {
			wildcard = true
		}
		allowSet[n] = true
	}
	denySet := make(map[string]bool, len(deny)+len(exclude))
	for _, n := range deny {
		denySet[n] = true
	}
	for _, n := range exclude {
		denySet[n] = true
	}

	out := NewRegistry()
	for name, t := range r.tools {
		if !wildcard && !allowSet[name] {
			continue
		}
		if denySet[name] {
			continue
		}
		out.tools[name] = t
	}
	return out
}
