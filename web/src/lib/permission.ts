// Per-session permission mode (execution-permission gate). The mode lives in the
// session's generic state store under "permission_mode": "auto" applies the
// server's env PERMISSION_* policy (dangerous calls gate for a yes/no card);
// "allow_all" bypasses that APPROVAL gate so no permission card appears (an env
// "deny" still blocks, and ask_user / client_tool are unaffected). The backend's
// permission middleware reads it at call time, so the toggle takes effect on the
// very next tool call — no run rebuild.
//
// Transport mirrors the plan (capability-gap O1): the client writes the key via
// POST /api/chat/sessions/{id}/state, and reads it back from the live
// `data-session-state` frame and the /api/chat/history sessionState echo. We
// capture the latest mode here (outside assistant-ui, like the plan/approval
// buses) so the header toggle and the tool-card gate render it.

import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";

export type PermissionMode = "auto" | "allow_all";

// The latest mode for the active session (null = not yet known; treated as auto).
let current: PermissionMode | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

// reportPermissionMode records the latest mode (from a live session-state frame
// or the history echo). Unknown/empty values normalize to null (= auto) so a
// forged frame can't widen the policy the user sees.
export function reportPermissionMode(mode: unknown) {
  const next: PermissionMode | null = mode === "allow_all" ? "allow_all" : null;
  if (next === current) return;
  current = next;
  emit();
}

// resetPermissionMode clears the mode on conversation switch. The switched-to
// session's echo re-reports its own mode on load. A NEW (blank) conversation has
// no echo, so it falls back to auto — 完全允许 is a per-session relaxation, never
// a default carried into the next chat.
export function resetPermissionMode() {
  if (current === null) return;
  current = null;
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function getSnapshot(): PermissionMode {
  return current ?? "auto";
}

// usePermissionMode returns the session's current mode (default auto).
export function usePermissionMode(): PermissionMode {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// permissionModeFromSessionState extracts the mode from a session-state
// frame/history entry: {key:"permission_mode", value:"allow_all"} → the value.
export function permissionModeFromSessionState(data: unknown): PermissionMode | null {
  const d = data as { key?: string; value?: unknown } | undefined;
  if (d?.key !== "permission_mode") return null;
  return d.value === "allow_all" ? "allow_all" : "auto";
}

// setPermissionMode persists the session's mode via the client state endpoint,
// then updates the local store. The write fans out a live session-state frame,
// so every attached client (and a reload) converges on the same mode.
export async function setPermissionMode(sessionId: string, mode: PermissionMode): Promise<boolean> {
  const token = getToken();
  if (!token || !sessionId) return false;
  const res = await fetch(`/api/chat/sessions/${encodeURIComponent(sessionId)}/state`, {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
    body: JSON.stringify({ key: "permission_mode", value: mode }),
  }).catch(() => null);
  if (res === null || !res.ok) return false;
  reportPermissionMode(mode);
  return true;
}

// usePermissionModeController couples the reactive mode with a setter for the
// active session. On a live session the choice persists via the endpoint and
// re-renders through the store. On a blank draft (no session yet) there is
// nothing to persist to, so the choice is held locally in the store and applied
// to the session the first message creates — see applyDraftPermissionMode in
// App.tsx, which reads the store at that moment (not a persisted draft).
export function usePermissionModeController(sessionId: string | null): [PermissionMode, (m: PermissionMode) => void] {
  const mode = usePermissionMode();
  const set = (m: PermissionMode) => {
    if (!sessionId) {
      // Draft: hold the choice locally so the toggle works before the first
      // message creates a session. In-memory only — it is NOT persisted, so it
      // can't leak into an unrelated future chat.
      current = m === "allow_all" ? "allow_all" : null;
      emit();
      return;
    }
    void setPermissionMode(sessionId, m);
  };
  return [mode, set];
}

// pendingDraftPermissionMode returns the in-memory draft choice (the mode picked
// on a blank draft, before a session exists), or null when auto. App.tsx reads
// this when the first session id arrives to apply the choice to that session.
export function pendingDraftPermissionMode(): PermissionMode | null {
  return current;
}
