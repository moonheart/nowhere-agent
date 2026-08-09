// A tiny in-app event bus feeding the right-hand panel. The chat thread is the
// source of truth for what the agent did; components there (tool calls, text)
// publish activity here, and the panel's tabs (Runs / Workspace) subscribe.
// Keeping it outside assistant-ui means the panel re-renders independently of
// the high-frequency streaming store.

import { useSyncExternalStore } from "react";

export type ToolActivity = {
  id: string;
  toolName: string;
  argsText: string;
  result?: unknown;
  isError?: boolean;
  status: "running" | "done" | "error";
  /** Epoch ms when the call started. */
  at: number;
};

// A subagent run aggregated from the backend's live `data-subagent` activity
// signals (start → stream/tool/result… → done/error). It is a UI progress hint
// only; the subagent's actual output reaches the model as the spawn_agent tool
// result.
//
// The run is kept as an ordered part list (thinking / text / tool-call) so the
// chat card can render it with the SAME components as a top-level assistant
// message — a subagent's own spawn_agent nesting recurses naturally.
export type SubPart =
  | { kind: "thinking"; text: string }
  | { kind: "text"; text: string }
  | {
      kind: "tool";
      id: string;
      toolName: string;
      argsText: string;
      result?: unknown;
      isError?: boolean;
      status: "running" | "done" | "error";
    };

export type SubagentRun = {
  id: number;
  agentType: string;
  depth: number;
  tools: string[]; // flat tool-name list for the compact Runs-panel row
  status: "running" | "done" | "error";
  at: number;
  /** The spawn_agent tool-call id this child belongs to (links to the chat card). */
  toolCallId?: string;
  /** Ordered content parts of the child's turn(s), streamed live. */
  parts: SubPart[];
};

export type SubagentSignal = {
  agentType: string;
  depth: number;
  phase: "start" | "stream" | "tool" | "result" | "interrupted" | "done" | "error";
  tool?: string;
  toolCallId?: string;
  /** "text" | "thinking" for stream signals; the interaction kind (e.g. "approval") for interrupted. */
  kind?: string;
  text?: string;
  subToolCallId?: string;
  args?: unknown;
  result?: unknown;
  isError?: boolean;
};

export type ActivityState = {
  /** Tool calls in the order they started, updated in place as they finish. */
  tools: ToolActivity[];
  /** Subagent runs spawned during this conversation. */
  subagents: SubagentRun[];
};

let state: ActivityState = { tools: [], subagents: [] };
let subagentSeq = 0;
// epoch bumps on every resetActivity. A gated tool call resumes on a FRESH
// backend run whose re-stream (decision-follow / poll-attach) can still be
// draining when the user switches or starts a new chat — its late report would
// otherwise re-add the just-cleared rows to the new conversation's panel.
// Reports captured against an older epoch are dropped.
let epoch = 0;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

function setState(next: ActivityState) {
  state = next;
  emit();
}

// reportToolCall upserts a tool activity by id: called as a call starts and
// again when its result lands, so the Runs tab reflects progress live. `atEpoch`
// is the conversation epoch the reporter is streaming for; a stale report (from
// a run whose conversation was already reset) is ignored.
export function reportToolCall(
  entry: Omit<ToolActivity, "at"> & { at?: number },
  atEpoch: number = epoch,
) {
  if (atEpoch !== epoch) return;
  const id = entry.id || `${entry.toolName}:${entry.at ?? 0}`;
  const existing = state.tools.findIndex((t) => t.id === id);
  const record: ToolActivity = {
    ...entry,
    id,
    at: entry.at ?? Date.now(),
    result: entry.result,
  };
  if (existing >= 0) {
    const tools = state.tools.slice();
    tools[existing] = { ...tools[existing], ...record };
    setState({ ...state, tools });
  } else {
    setState({ ...state, tools: [...state.tools, record] });
  }
}

// reportSubagentActivity folds one live subagent signal into the run's ordered
// part list: `start` opens a run, `stream` appends to the trailing thinking/text
// part, `tool` opens a tool part, `result` fills it, `done`/`error` closes the
// run. Runs are matched by toolCallId when present (parallel subagents), falling
// back to most-recent running run at that depth.
export function reportSubagentActivity(sig: SubagentSignal, atEpoch: number = epoch) {
  if (atEpoch !== epoch) return;
  const runs = state.subagents.slice();
  if (sig.phase === "start") {
    runs.push({
      id: subagentSeq++,
      agentType: sig.agentType,
      depth: sig.depth,
      tools: [],
      status: "running",
      at: Date.now(),
      toolCallId: sig.toolCallId,
      parts: [],
    });
  } else {
    const idx = findRun(runs, sig);
    if (idx < 0) return; // stray signal (start dropped); ignore
    const run = { ...runs[idx], parts: runs[idx].parts.slice(), tools: runs[idx].tools.slice() };
    switch (sig.phase) {
      case "stream":
        if (sig.text) appendStream(run, sig.kind === "thinking" ? "thinking" : "text", sig.text);
        break;
      case "tool":
        if (sig.tool) {
          run.tools.push(sig.tool);
          run.parts.push({
            kind: "tool",
            id: sig.subToolCallId || `${sig.tool}:${run.parts.length}`,
            toolName: sig.tool,
            argsText: sig.args ? JSON.stringify(sig.args) : "",
            status: "running",
          });
        }
        break;
      case "result":
        fillToolResult(run, sig);
        break;
      case "interrupted":
        // The child hit a gate it cannot resolve (subagents can't suspend for
        // human input) and stopped. Show it as failed with the reason, not as
        // still running — the backend suppresses the trailing "done" here.
        run.status = "error";
        closeOpenTools(run);
        run.parts.push({
          kind: "text",
          text: sig.tool
            ? `Stopped: needs ${sig.kind || "approval"} for ${sig.tool}, which cannot be delivered inside a subagent.`
            : "Stopped: waiting on input that cannot be delivered inside a subagent.",
        });
        break;
      case "done":
        run.status = "done";
        closeOpenTools(run);
        break;
      case "error":
        run.status = "error";
        closeOpenTools(run);
        break;
    }
    runs[idx] = run;
  }
  setState({ ...state, subagents: runs.slice(-50) });
}

// appendStream appends a delta to the trailing part of the same kind, starting
// a new part when the kind changes (preserves think/text order like a top turn).
function appendStream(run: SubagentRun, kind: "thinking" | "text", delta: string) {
  const last = run.parts[run.parts.length - 1];
  if (last && last.kind === kind) {
    run.parts[run.parts.length - 1] = { ...last, text: last.text + delta };
  } else {
    run.parts.push({ kind, text: delta });
  }
}

// fillToolResult matches a child tool-result back onto its tool part by id (or
// the latest still-running tool part), marking it done/error with its output.
function fillToolResult(run: SubagentRun, sig: SubagentSignal) {
  for (let i = run.parts.length - 1; i >= 0; i--) {
    const p = run.parts[i];
    if (p.kind !== "tool") continue;
    const idMatch = sig.subToolCallId && p.id === sig.subToolCallId;
    const fallback = !sig.subToolCallId && p.status === "running";
    if (idMatch || fallback) {
      run.parts[i] = {
        ...p,
        result: sig.result,
        isError: sig.isError,
        status: sig.isError ? "error" : "done",
      };
      return;
    }
  }
}

function closeOpenTools(run: SubagentRun) {
  run.parts = run.parts.map((p) =>
    p.kind === "tool" && p.status === "running" ? { ...p, status: "done" } : p,
  );
}

// findRun locates the running subagent a signal belongs to: by toolCallId when
// the signal carries one (parallel spawns), else the most recent running run at
// the same depth (legacy signals without an id).
function findRun(runs: SubagentRun[], sig: SubagentSignal): number {
  if (sig.toolCallId) {
    for (let i = runs.length - 1; i >= 0; i--) {
      if (runs[i].toolCallId === sig.toolCallId) return i;
    }
  }
  for (let i = runs.length - 1; i >= 0; i--) {
    if (runs[i].status === "running" && runs[i].depth === sig.depth) return i;
  }
  return -1;
}

// resetActivity clears per-conversation activity when switching threads so the
// panel doesn't show a previous chat's runs. Bumping the epoch also invalidates
// any report still in flight from the old conversation's streams.
export function resetActivity() {
  epoch++;
  setState({ tools: [], subagents: [] });
}

// activityEpoch exposes the current conversation epoch so a streaming reporter
// (tool-call card, subagent feed) can tag its reports and have them dropped
// once the conversation resets underneath it.
export function activityEpoch(): number {
  return epoch;
}

export function subscribeActivity(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

export function getActivity(): ActivityState {
  return state;
}

// useActivity subscribes a component to the activity feed.
export function useActivity(): ActivityState {
  return useSyncExternalStore(subscribeActivity, getActivity);
}
