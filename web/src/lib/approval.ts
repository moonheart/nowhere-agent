// Tool-approval prompts (capability-gap O2). The backend parks a run on a
// dangerous tool call and streams a transient `data-tool-approval` frame; we
// capture it here (outside assistant-ui, like the activity bus) so the tool
// card can render approve/deny. Deciding POSTs the verdict to the backend,
// which resumes the run — the existing multi-client resume poll picks it up.

import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";

export type ToolApproval = {
  approvalId: string;
  toolCallId: string;
  toolName: string;
  args?: unknown;
};

// Pending approvals keyed by toolCallId (the chat card's stable handle).
let pending = new Map<string, ToolApproval>();
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

// reportApproval registers a pending approval from a data-tool-approval frame.
export function reportApproval(a: ToolApproval) {
  if (!a.approvalId || !a.toolCallId) return;
  pending = new Map(pending).set(a.toolCallId, a);
  emit();
}

// clearApproval drops a toolCallId's prompt after the user decided (the backend
// resumed the run; the card re-renders from the resumed stream).
export function clearApproval(toolCallId: string) {
  if (!pending.has(toolCallId)) return;
  pending = new Map(pending);
  pending.delete(toolCallId);
  emit();
}

// resetApprovals clears all prompts (new conversation / session switch).
export function resetApprovals() {
  if (pending.size === 0) return;
  pending = new Map();
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const EMPTY: ToolApproval[] = [];
let snapshot: ToolApproval[] = EMPTY;
function getSnapshot(): ToolApproval[] {
  // Stable identity across unrelated emits so useSyncExternalStore doesn't loop.
  const cur = Array.from(pending.values());
  if (
    snapshot.length === cur.length &&
    cur.every((c, i) => snapshot[i] === c)
  ) {
    return snapshot;
  }
  snapshot = cur;
  return snapshot;
}

// useApprovals returns the current pending approvals (re-renders on change).
export function useApprovals(): ToolApproval[] {
  return useSyncExternalStore(subscribe, getSnapshot);
}

// useApproval returns the pending approval for one tool call, or undefined.
export function useApproval(toolCallId: string | undefined): ToolApproval | undefined {
  const all = useApprovals();
  if (!toolCallId) return undefined;
  return all.find((a) => a.toolCallId === toolCallId);
}

// respondToApproval POSTs the human verdict to the backend, which resumes the
// parked run. Returns true when the backend accepted it.
export async function respondToApproval(
  approvalId: string,
  approved: boolean,
): Promise<boolean> {
  const token = getToken();
  if (!token) return false;
  const res = await fetch("/api/chat/approval", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ approvalId, approved }),
  }).catch(() => null);
  return res !== null && res.ok;
}
