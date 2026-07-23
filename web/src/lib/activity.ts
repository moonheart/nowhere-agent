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
// signals (start → tool… → done/error). It is a UI progress hint only; the
// subagent's actual output reaches the model as the spawn_agent tool result.
export type SubagentRun = {
  id: number;
  agentType: string;
  depth: number;
  tools: string[];
  status: "running" | "done" | "error";
  at: number;
};

export type SubagentSignal = {
  agentType: string;
  depth: number;
  phase: "start" | "tool" | "done" | "error";
  tool?: string;
};

export type ActivityState = {
  /** Tool calls in the order they started, updated in place as they finish. */
  tools: ToolActivity[];
  /** Subagent runs spawned during this conversation. */
  subagents: SubagentRun[];
};

let state: ActivityState = { tools: [], subagents: [] };
let subagentSeq = 0;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

function setState(next: ActivityState) {
  state = next;
  emit();
}

// reportToolCall upserts a tool activity by id: called as a call starts and
// again when its result lands, so the Runs tab reflects progress live.
export function reportToolCall(entry: Omit<ToolActivity, "at"> & { at?: number }) {
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

// reportSubagentActivity folds one live subagent signal into the aggregated run
// list: `start` opens a run, `tool` appends to the most recent running run at
// that depth, `done`/`error` closes it. Depth matching keeps a grandchild's
// tools off its parent's entry.
export function reportSubagentActivity(sig: SubagentSignal) {
  const runs = state.subagents.slice();
  if (sig.phase === "start") {
    runs.push({
      id: subagentSeq++,
      agentType: sig.agentType,
      depth: sig.depth,
      tools: [],
      status: "running",
      at: Date.now(),
    });
  } else {
    let idx = -1;
    for (let i = runs.length - 1; i >= 0; i--) {
      if (runs[i].status === "running" && runs[i].depth === sig.depth) {
        idx = i;
        break;
      }
    }
    if (idx < 0) return; // stray signal (start dropped); ignore
    const run = { ...runs[idx] };
    if (sig.phase === "tool" && sig.tool) run.tools = [...run.tools, sig.tool];
    else if (sig.phase === "done") run.status = "done";
    else if (sig.phase === "error") run.status = "error";
    runs[idx] = run;
  }
  setState({ ...state, subagents: runs.slice(-50) });
}

// resetActivity clears per-conversation activity when switching threads so the
// panel doesn't show a previous chat's runs.
export function resetActivity() {
  setState({ tools: [], subagents: [] });
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
