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

export type ActivityState = {
  /** Tool calls in the order they started, updated in place as they finish. */
  tools: ToolActivity[];
};

let state: ActivityState = { tools: [] };
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
    setState({ tools });
  } else {
    setState({ tools: [...state.tools, record] });
  }
}

// resetActivity clears per-conversation activity when switching threads so the
// panel doesn't show a previous chat's runs.
export function resetActivity() {
  setState({ tools: [] });
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
