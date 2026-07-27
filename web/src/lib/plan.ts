// Plan/TODO tracking (capability-gap O1). The backend's plan_write tool persists
// the plan as the "plan" key of the session's generic state store and pushes it
// live via a `data-session-state` frame; we capture the latest plan here
// (outside assistant-ui, like the approval/activity buses) so the top plan panel
// can render it. On reload, /api/chat/history echoes the same state and load()
// re-reports it — so the panel survives a refresh.

import { useSyncExternalStore } from "react";

export type PlanItem = {
  content: string;
  status: "pending" | "in_progress" | "completed";
  activeForm?: string;
};

export type Plan = {
  items: PlanItem[];
};

// The latest plan for the active session (null = none recorded yet).
let current: Plan | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

// reportPlan records the latest plan (from a live session-state frame or the
// history echo). A null/empty argument is ignored — the panel only clears on an
// explicit resetPlan (new conversation / session switch), so a stale empty frame
// can't blank a real plan.
export function reportPlan(p: Plan | null | undefined) {
  if (!p || !Array.isArray(p.items)) return;
  current = p;
  emit();
}

// resetPlan clears the panel (new conversation / session switch).
export function resetPlan() {
  if (current === null) return;
  current = null;
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function getSnapshot(): Plan | null {
  return current;
}

// usePlan returns the session's latest plan, or null if none.
export function usePlan(): Plan | null {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// pickPlanState extracts the plan from a session-state frame/history entry:
// {key:"plan", value:{items:[...]}} → the value, or null for other keys.
export function planFromSessionState(data: unknown): Plan | null {
  const d = data as { key?: string; value?: Plan } | undefined;
  if (d?.key !== "plan" || !d.value || !Array.isArray(d.value.items)) return null;
  return d.value;
}
