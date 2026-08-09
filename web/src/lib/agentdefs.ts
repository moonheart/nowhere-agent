// Typed client for the agent-definition management endpoints
// (persist-agent-defs). One function per route, grouped by tier, so a
// component never assembles a URL. Scopes:
//   self     /api/me/agentdefs          — the caller's own definitions
//   team     /api/teams/{id}/agentdefs  — that team's shared defs (role-gated)
//   platform /api/admin/agentdefs       — system (global) defs (admin only)

import { api } from "@/lib/api";

export type AgentDefScope = "user" | "team" | "system";

export type AgentDef = {
  id: string;
  name: string;
  scope: AgentDefScope;
  user_id?: string;
  team_id?: string;
  current_version: number;
  description: string;
  tools: string[];
  disallowedTools: string[];
  skills: string[];
  model?: string;
  maxTurns?: number;
  document: string;
  created_by?: string;
  created_at: string;
  updated_at: string;
};

// A base path identifies the scope a call operates in.
export type AgentDefBase =
  | { kind: "me" }
  | { kind: "team"; teamId: string }
  | { kind: "platform" };

function basePath(b: AgentDefBase): string {
  switch (b.kind) {
    case "me":
      return "/api/me/agentdefs";
    case "team":
      return `/api/teams/${encodeURIComponent(b.teamId)}/agentdefs`;
    case "platform":
      return "/api/admin/agentdefs";
  }
}

const enc = encodeURIComponent;

// SaveResult carries the write response: the stored def plus any server-side
// warnings (e.g. declared skills with no available runner).
export type SaveResult = { def: AgentDef; warnings?: string[] };

export const listAgentDefs = (b: AgentDefBase) => api<{ defs: AgentDef[] }>(basePath(b));

export const getAgentDef = (b: AgentDefBase, name: string) =>
  api<{ def: AgentDef }>(`${basePath(b)}/${enc(name)}`);

export const createAgentDef = (b: AgentDefBase, document: string) =>
  api<SaveResult>(basePath(b), { method: "POST", body: { document } });

export const updateAgentDef = (b: AgentDefBase, name: string, document: string) =>
  api<SaveResult>(`${basePath(b)}/${enc(name)}`, { method: "PUT", body: { document } });

export const deleteAgentDef = (b: AgentDefBase, name: string) =>
  api<void>(`${basePath(b)}/${enc(name)}`, { method: "DELETE" });
