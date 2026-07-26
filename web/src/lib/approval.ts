// Human-interaction prompts (capability-gap O2 + O-ask). The backend parks a run
// on a gated tool call — a dangerous action needing approval, or an ask_user
// question set — and streams a transient `data-tool-approval` frame; we capture
// it here (outside assistant-ui, like the activity bus) so the tool card can
// render the right UI. Deciding POSTs the verdict to the backend, which resumes
// the run — the existing multi-client resume poll picks it up.

import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";

export type AskOption = {
  label: string;
  description?: string;
  recommended?: boolean;
};

export type AskQuestion = {
  question: string;
  header?: string;
  multiselect?: boolean;
  options: AskOption[];
};

export type ToolApproval = {
  approvalId: string;
  toolCallId: string;
  toolName: string;
  // kind: "approval" (dangerous call, yes/no) or "ask_user" (question card).
  kind?: "approval" | "ask_user";
  args?: unknown;
};

// Pending interactions keyed by toolCallId (the chat card's stable handle).
let pending = new Map<string, ToolApproval>();
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

// reportApproval registers a pending interaction from a data-tool-approval frame.
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
  const cur = Array.from(pending.values());
  if (snapshot.length === cur.length && cur.every((c, i) => snapshot[i] === c)) {
    return snapshot;
  }
  snapshot = cur;
  return snapshot;
}

// useApproval returns the pending interaction for one tool call, or undefined.
export function useApproval(toolCallId: string | undefined): ToolApproval | undefined {
  const all = useSyncExternalStore(subscribe, getSnapshot);
  if (!toolCallId) return undefined;
  return all.find((a) => a.toolCallId === toolCallId);
}

// respondToApproval POSTs a permission-approval verdict (approve/deny).
export async function respondToApproval(approvalId: string, approved: boolean): Promise<boolean> {
  return postDecision(approvalId, { approved });
}

// respondToAskUser POSTs an ask_user answer. answers maps each question to the
// chosen label(s) or a custom string; null answers + approved=false skips.
export async function respondToAskUser(
  approvalId: string,
  answers: Record<string, string | string[]> | null,
): Promise<boolean> {
  if (answers === null) {
    return postDecision(approvalId, { approved: false });
  }
  return postDecision(approvalId, { approved: true, answer: { answers } });
}

async function postDecision(approvalId: string, body: Record<string, unknown>): Promise<boolean> {
  const token = getToken();
  if (!token) return false;
  const res = await fetch("/api/chat/approval", {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
    body: JSON.stringify({ approvalId, ...body }),
  }).catch(() => null);
  if (res !== null && res.ok) {
    noteDecision();
    return true;
  }
  return false;
}

// lastDecisionAt marks when THIS client last posted a verdict. The resumed run
// belongs to this same session, so the multi-client attach poll (which live-
// follows runs started elsewhere) must not also attach to it — that would start
// a second, divergent copy of the reply. recentDecision() lets the poll skip
// attaching right after a decision.
let lastDecisionAt = 0;
const DECISION_ATTACH_SUPPRESS_MS = 10_000;

function noteDecision() {
  lastDecisionAt = Date.now();
}

// recentDecision reports whether this client decided an interaction moments ago
// (so its run is resuming and the attach poll should leave it alone).
export function recentDecision(): boolean {
  return Date.now() - lastDecisionAt < DECISION_ATTACH_SUPPRESS_MS;
}

// parseQuestions extracts the ask_user question set from a ToolApproval.args
// (the model's tool input). Returns [] for non-ask_user or malformed input.
export function parseQuestions(a: ToolApproval): AskQuestion[] {
  const args = a.args as { questions?: AskQuestion[] } | undefined;
  if (!args || !Array.isArray(args.questions)) return [];
  return args.questions.filter((q) => q && typeof q.question === "string" && Array.isArray(q.options));
}
