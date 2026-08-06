// Typed client for the skill-management endpoints (skill-console). One function
// per route, grouped by tier, so a component never assembles a URL. Scopes:
//   self     /api/me/skills          — the caller's own user skills
//   team     /api/teams/{id}/skills  — that team's shared skills (role-gated)
//   platform /api/admin/skills       — system (global) skills (admin only)

import { api } from "@/lib/api";

export type SkillScope = "user" | "team" | "system";

export type Skill = {
  id: string;
  name: string;
  scope: SkillScope;
  user_id?: string;
  team_id?: string;
  current_version: number;
  overrides_version: number;
  description: string;
  body: string;
  resources: Record<string, string>;
  scripts: Record<string, string>;
  needs_review: boolean;
  created_at: string;
  updated_at: string;
};

export type SkillVersion = {
  version: number;
  created_by?: string;
  created_at: string;
};

// SkillInput is the create/update payload (the scope comes from the route).
export type SkillInput = {
  name: string;
  description: string;
  body: string;
  resources: Record<string, string>;
  scripts: Record<string, string>;
  overrides_version?: number;
};

// A base path identifies the scope a call operates in.
export type SkillBase =
  | { kind: "me" }
  | { kind: "team"; teamId: string }
  | { kind: "platform" };

function basePath(b: SkillBase): string {
  switch (b.kind) {
    case "me":
      return "/api/me/skills";
    case "team":
      return `/api/teams/${encodeURIComponent(b.teamId)}/skills`;
    case "platform":
      return "/api/admin/skills";
  }
}

const enc = encodeURIComponent;

export const listSkills = (b: SkillBase) => api<{ skills: Skill[] }>(basePath(b));

export const createSkill = (b: SkillBase, body: SkillInput) =>
  api<{ skill: Skill }>(basePath(b), { method: "POST", body });

export const getSkill = (b: SkillBase, id: string) =>
  api<{ skill: Skill }>(`${basePath(b)}/${enc(id)}`);

export const updateSkill = (b: SkillBase, id: string, body: SkillInput) =>
  api<{ skill: Skill }>(`${basePath(b)}/${enc(id)}`, { method: "PUT", body });

export const deleteSkill = (b: SkillBase, id: string) =>
  api<void>(`${basePath(b)}/${enc(id)}`, { method: "DELETE" });

export const skillVersions = (b: SkillBase, id: string) =>
  api<{ versions: SkillVersion[] }>(`${basePath(b)}/${enc(id)}/versions`);

export const skillVersionAt = (b: SkillBase, id: string, v: number) =>
  api<{ skill: Skill }>(`${basePath(b)}/${enc(id)}/versions/${v}`);

export const rollbackSkill = (b: SkillBase, id: string, v: number) =>
  api<{ skill: Skill }>(`${basePath(b)}/${enc(id)}/rollback/${v}`, { method: "POST" });
