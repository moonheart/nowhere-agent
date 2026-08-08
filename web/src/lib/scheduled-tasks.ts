// Typed client for the scheduled-task endpoints (scheduled-tasks). One function
// per route so a component never assembles a URL. All routes are owner-scoped:
// they live under /api/me/scheduled-tasks and manage only the caller's own
// tasks, so there is no scope/team base variant (unlike skills).

import { api } from "@/lib/api";

export type MultitaskStrategy = "reject" | "interrupt" | "enqueue";
export type OnRunCompleted = "keep" | "delete";

// ScheduledTask is one recurring agent run.
export type ScheduledTask = {
  id: string;
  agent_def_name?: string;
  prompt?: string;
  tool_whitelist: string[];
  cron: string;
  timezone: string;
  target_session_id?: string;
  on_run_completed: OnRunCompleted;
  multitask_strategy: MultitaskStrategy;
  end_time?: string;
  enabled: boolean;
  next_run_at: string;
  last_run_at?: string;
  metadata?: Record<string, unknown>;
  created_at: string;
  updated_at: string;
};

// ScheduledTaskInput is the create/update payload (the owner comes from the
// caller, never the body). Enum fields may be omitted to take the backend's
// defaults (on_run_completed=keep, multitask_strategy=reject).
export type ScheduledTaskInput = {
  agent_def_name?: string;
  prompt?: string;
  tool_whitelist: string[];
  cron: string;
  timezone: string;
  target_session_id?: string;
  on_run_completed?: OnRunCompleted;
  multitask_strategy?: MultitaskStrategy;
  end_time?: string;
  metadata?: Record<string, unknown>;
};

const BASE = "/api/me/scheduled-tasks";
const enc = encodeURIComponent;

export const listScheduledTasks = () => api<{ tasks: ScheduledTask[] }>(BASE);

export const createScheduledTask = (body: ScheduledTaskInput) =>
  api<{ task: ScheduledTask }>(BASE, { method: "POST", body });

export const getScheduledTask = (id: string) =>
  api<{ task: ScheduledTask }>(`${BASE}/${enc(id)}`);

export const updateScheduledTask = (id: string, body: ScheduledTaskInput) =>
  api<{ task: ScheduledTask }>(`${BASE}/${enc(id)}`, { method: "PUT", body });

export const deleteScheduledTask = (id: string) =>
  api<void>(`${BASE}/${enc(id)}`, { method: "DELETE" });

export const enableScheduledTask = (id: string) =>
  api<{ task: ScheduledTask }>(`${BASE}/${enc(id)}/enable`, { method: "POST" });

export const disableScheduledTask = (id: string) =>
  api<{ task: ScheduledTask }>(`${BASE}/${enc(id)}/disable`, { method: "POST" });

// taskSessions lists the session ids a task has produced (the fire created one
// per run when the task has no fixed target session).
export const taskSessions = (id: string) =>
  api<{ sessions: string[] }>(`${BASE}/${enc(id)}/sessions`);

// clearTaskSessions soft-deletes every session a task produced (they leave the
// sidebar but their rows stay for audit). Returns how many were cleared.
export const clearTaskSessions = (id: string) =>
  api<{ cleared: number }>(`${BASE}/${enc(id)}/sessions/clear`, { method: "POST" });
