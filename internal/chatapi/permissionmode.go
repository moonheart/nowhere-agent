package chatapi

// Permission-mode session-state key and values. The mode lives in the session's
// generic state store (capability-gap O1), so it is per-session, durable, and
// fans out live to clients via the same session_state frames as the plan.
const (
	// PermissionModeStateKey is the session-state key holding the mode.
	PermissionModeStateKey = "permission_mode"
	// PermissionModeAuto applies the server's env PERMISSION_* policy as-is.
	PermissionModeAuto = "auto"
	// PermissionModeAllowAll bypasses the permission-APPROVAL gate: dangerous
	// calls the policy would gate for a yes/no run without prompting. It never
	// widens access — a policy "deny" still blocks the call — and never touches
	// ask_user / client_tool interactions (those are not permission approvals).
	PermissionModeAllowAll = "allow_all"
)
