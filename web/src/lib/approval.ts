// Client-interaction prompts (general interrupt). The backend parks a run on a
// tool call that needs the client — a dangerous action needing approval, an
// ask_user question set, or a CLIENT-SIDE tool the browser executes — and
// streams a transient `data-interaction` frame; we capture it here (outside
// assistant-ui, like the activity bus) so the tool card can render the right UI.
// Deciding POSTs the verdict to the backend, which resumes the run — the
// existing multi-client resume poll picks it up.

import { useSyncExternalStore } from "react";
import { getToken } from "@/lib/auth";
import { getSessionId } from "@/lib/thread";
import { ApiError } from "@/lib/api";
import { runClientTool } from "@/lib/client-tools";

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

export type Interaction = {
  // interactionId is the durable interaction's id (approvalId kept as a legacy
  // alias for frames/history produced before the rename).
  interactionId: string;
  approvalId?: string;
  toolCallId: string;
  toolName: string;
  // kind: "approval" (dangerous call, yes/no), "ask_user" (question card), or
  // "client_tool" (the browser executes it and returns the output).
  kind?: "approval" | "ask_user" | "client_tool";
  args?: unknown;
};

// ToolApproval is retained as an alias of Interaction for the card components.
export type ToolApproval = Interaction;

// Pending interactions keyed by toolCallId (the chat card's stable handle).
let pending = new Map<string, Interaction>();
const listeners = new Set<() => void>();

// autoRan tracks client_tool interaction ids already auto-executed, so the same
// frame arriving twice (live stream + a reload's pendingApproval echo) runs the
// browser capability and POSTs its output only once.
const autoRan = new Set<string>();

// failed tracks client_tool interactions whose auto-run or verdict POST failed,
// keyed by toolCallId → reason. The indicator card renders the reason instead of
// spinning "Running…" forever; the prompt stays until the run expires.
let failed = new Map<string, string>();

function emit() {
  for (const l of listeners) l();
}

// reportInteraction registers a pending interaction from a data-interaction
// frame. A client_tool interaction is auto-executed (the browser runs the named
// capability) and its output POSTed — no human click unless the capability
// itself prompts.
export function reportInteraction(a: Interaction) {
  const id = a.interactionId || a.approvalId || "";
  if (!id || !a.toolCallId) return;
  const norm = { ...a, interactionId: id };
  pending = new Map(pending).set(a.toolCallId, norm);
  emit();
  if (norm.kind === "client_tool" && !autoRan.has(id)) {
    autoRan.add(id);
    void executeClientTool(norm);
  }
}

// executeClientTool runs a client_tool interaction's capability in the browser
// and POSTs the output (or error) as the verdict, resuming the run.
async function executeClientTool(a: Interaction) {
  try {
    const result = await runClientTool(a.toolName, a.args);
    const stream = await respondToClientTool(a.interactionId, result);
    if (stream) {
      clearApproval(a.toolCallId);
      if (!hasPendingInteractions()) followDecisionStream(stream);
    }
  } catch (err) {
    // The verdict POST failed (network or server error): the run stays parked,
    // so mark the card with the reason instead of leaving it running forever.
    markClientToolFailed(a.toolCallId, (err as Error).message || "the verdict could not be sent");
  }
}

// markClientToolFailed records why a client_tool interaction's auto-run or
// verdict POST failed, so its card can render an error state.
function markClientToolFailed(toolCallId: string, message: string) {
  failed = new Map(failed).set(toolCallId, message);
  emit();
}

// clearApproval drops a toolCallId's prompt after the user decided (the backend
// resumed the run; the card re-renders from the resumed stream).
export function clearApproval(toolCallId: string) {
  if (!pending.has(toolCallId) && !failed.has(toolCallId)) return;
  pending = new Map(pending);
  pending.delete(toolCallId);
  failed = new Map(failed);
  failed.delete(toolCallId);
  emit();
}

// hasPendingInteractions reports whether any card is still parked. After a
// verdict the deciding client uses it to tell a real resume (batch complete → a
// fresh run streams) from a no-op (siblings still queued → the backend did NOT
// start a run), so it only attaches the runtime's follower to an actual run and
// never opens an empty assistant bubble for a still-waiting batch.
export function hasPendingInteractions(): boolean {
  return pending.size > 0;
}

// resetApprovals clears all prompts (new conversation / session switch).
export function resetApprovals() {
  autoRan.clear();
  if (pending.size === 0 && failed.size === 0) return;
  pending = new Map();
  failed = new Map();
  emit();
}

function subscribe(fn: () => void) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const EMPTY: Interaction[] = [];
let snapshot: Interaction[] = EMPTY;
function getSnapshot(): Interaction[] {
  const cur = Array.from(pending.values());
  if (snapshot.length === cur.length && cur.every((c, i) => snapshot[i] === c)) {
    return snapshot;
  }
  snapshot = cur;
  return snapshot;
}

const EMPTY_FAILED: { toolCallId: string; message: string }[] = [];
let failedSnapshot: { toolCallId: string; message: string }[] = EMPTY_FAILED;
function getFailedSnapshot(): { toolCallId: string; message: string }[] {
  const cur = Array.from(failed.entries()).map(([toolCallId, message]) => ({ toolCallId, message }));
  if (
    failedSnapshot.length === cur.length &&
    cur.every((c, i) => failedSnapshot[i].toolCallId === c.toolCallId && failedSnapshot[i].message === c.message)
  ) {
    return failedSnapshot;
  }
  failedSnapshot = cur;
  return failedSnapshot;
}

// useApproval returns the pending interaction for one tool call, or undefined.
export function useApproval(toolCallId: string | undefined): Interaction | undefined {
  const all = useSyncExternalStore(subscribe, getSnapshot);
  if (!toolCallId) return undefined;
  return all.find((a) => a.toolCallId === toolCallId);
}

// useApprovalFailure returns why a client_tool interaction's auto-run/verdict
// POST failed, or undefined while it is still running.
export function useApprovalFailure(toolCallId: string | undefined): string | undefined {
  useSyncExternalStore(subscribe, getFailedSnapshot);
  if (!toolCallId) return undefined;
  return failed.get(toolCallId);
}

// usePendingInteractions returns the full pending queue in batch (insertion)
// order. The head ([0]) is the one the user should act on first; a gated batch
// surfaces several cards but only the head is actionable — deciding it clears it
// and promotes the next. Mirrors the claude-code / pi sequential-permission UX.
export function usePendingInteractions(): Interaction[] {
  return useSyncExternalStore(subscribe, getSnapshot);
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

// respondToClientTool POSTs a client_tool result (the browser-executed output,
// or an error). The output is validated server-side against the tool's declared
// output schema before being folded as the tool result.
export async function respondToClientTool(
  interactionId: string,
  result: { output?: unknown; error?: string },
): Promise<ReadableStream<Uint8Array> | null> {
  return postDecision(interactionId, { approved: true, answer: result });
}

// postDecision sends the verdict through the chat endpoint (an `approval` field
// turns POST /api/chat into a resume instead of a new turn) and returns the SSE
// body streaming the run's continuation — the same attach path a normal turn
// uses, so no separate decision endpoint or polling is needed. A network or
// server error THROWS (ApiError carrying the server's `error` field) so gates
// can surface the reason; a success with no stream body returns null (the
// verdict was accepted but there is nothing to follow).
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
  if (res === null) {
    throw new ApiError("the verdict could not reach the server", 0);
  }
  if (!res.ok) {
    let msg = `request failed (${res.status})`;
    try {
      const data = (await res.json()) as { error?: string };
      if (data.error) msg = data.error;
    } catch {
      // non-JSON error body; keep the status fallback
    }
    throw new ApiError(msg, res.status);
  }
  if (!res.body) return null;
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

// parseQuestions extracts the ask_user question set from a Interaction.args
// (the model's tool input). Returns [] for non-ask_user or malformed input.
export function parseQuestions(a: Interaction): AskQuestion[] {
  const args = a.args as { questions?: AskQuestion[] } | undefined;
  if (!args || !Array.isArray(args.questions)) return [];
  return args.questions.filter((q) => q && typeof q.question === "string" && Array.isArray(q.options));
}
