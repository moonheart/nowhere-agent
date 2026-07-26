// Human-interaction prompts (capability-gap O2 + O-ask). The backend parks a run
// on a gated tool call — a dangerous action needing approval, or an ask_user
// question set — and streams a transient `data-tool-approval` frame; we capture
// it here (outside assistant-ui, like the activity bus) so the tool card can
// render the right UI. Deciding POSTs the verdict to the backend, which resumes
// the run — the existing multi-client resume poll picks it up.

import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";
import { getSessionId } from "@/lib/thread";

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

// respondToApproval POSTs a permission-approval verdict (approve/deny). Returns
// the resumed run's ui-message-stream body for the caller to follow live.
export async function respondToApproval(
  approvalId: string,
  approved: boolean,
): Promise<ReadableStream<Uint8Array> | null> {
  return postDecision(approvalId, { approved });
}

// respondToAskUser POSTs an ask_user answer. answers maps each question to the
// chosen label(s) or a custom string; null answers + approved=false skips.
export async function respondToAskUser(
  approvalId: string,
  answers: Record<string, string | string[]> | null,
): Promise<ReadableStream<Uint8Array> | null> {
  if (answers === null) {
    return postDecision(approvalId, { approved: false });
  }
  return postDecision(approvalId, { approved: true, answer: { answers } });
}

// postDecision sends the verdict through the chat endpoint (an `approval` field
// turns POST /api/chat into a resume instead of a new turn) and returns the SSE
// body streaming the run's continuation — the same attach path a normal turn
// uses, so no separate decision endpoint or polling is needed.
async function postDecision(
  approvalId: string,
  verdict: Record<string, unknown>,
): Promise<ReadableStream<Uint8Array> | null> {
  const token = getToken();
  if (!token) return null;
  const res = await fetch("/api/chat", {
    method: "POST",
    headers: { "content-type": "application/json", authorization: `Bearer ${token}` },
    body: JSON.stringify({ threadId: getSessionId(), approval: { approvalId, ...verdict } }),
  }).catch(() => null);
  if (res === null || !res.ok || !res.body) return null;
  return res.body;
}

// A DecisionStreamFollower attaches a verdict's returned SSE stream to the chat
// runtime so the deciding client watches the resumed run live. App registers it
// (it owns the runtime); the tool-call gates call it after a successful decide.
export type DecisionStreamFollower = (stream: ReadableStream<Uint8Array>) => void;

let follower: DecisionStreamFollower | null = null;

// registerDecisionFollower wires the runtime's follow fn. Called once by App.
export function registerDecisionFollower(fn: DecisionStreamFollower) {
  follower = fn;
}

// followDecisionStream hands a verdict stream to the registered follower.
export function followDecisionStream(stream: ReadableStream<Uint8Array>) {
  follower?.(stream);
}

// parseQuestions extracts the ask_user question set from a ToolApproval.args
// (the model's tool input). Returns [] for non-ask_user or malformed input.
export function parseQuestions(a: ToolApproval): AskQuestion[] {
  const args = a.args as { questions?: AskQuestion[] } | undefined;
  if (!args || !Array.isArray(args.questions)) return [];
  return args.questions.filter((q) => q && typeof q.question === "string" && Array.isArray(q.options));
}
