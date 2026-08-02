// Typed client for the management console's endpoints (admin-console). One
// function per route, grouped by tier, so a component never assembles a URL.

import { api, qs } from "@/lib/api";

// ---- shared shapes ----

export type PlatformRole = "user" | "admin";
export type TeamRole = "owner" | "admin" | "member";

export type User = {
  id: string;
  email: string;
  display_name: string;
  platform_role: PlatformRole;
  disabled: boolean;
  disabled_at?: string;
  created_at: string;
};

export type TeamMembership = { id: string; name: string; role: TeamRole };

export type Me = { user: User; teams: TeamMembership[] };

export type Team = {
  id: string;
  name: string;
  role?: TeamRole;
  members?: number;
  created_at: string;
};

export type Member = {
  user_id: string;
  email: string;
  display_name: string;
  role: TeamRole;
  disabled: boolean;
  joined_at: string;
};

export type TeamKey = {
  provider: string;
  masked: string;
  created_at: string;
  updated_at: string;
};

export type Tokens = {
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  runs: number;
};

export type UsageRow = { id: string; label: string; tokens: Tokens };

export type UsageReport = {
  total: Tokens;
  daily: UsageRow[];
  rows?: UsageRow[];
  group_by?: "user" | "team";
  // approximate/note carry the team-attribution caveat the server attaches to
  // any team-grouped figure. Rendering the numbers without them would present
  // an approximation as exact.
  approximate?: boolean;
  note?: string;
};

export type Memory = {
  id: string;
  scope: "user" | "team" | "system";
  user_id?: string;
  team_id?: string;
  kind: string;
  content: string;
  deprecated: boolean;
  created_at: string;
  updated_at: string;
};

export type SessionToken = {
  id: string;
  created_at: string;
  expires_at: string;
  current: boolean;
};

export type DateRange = { from?: string; to?: string };

// ---- self service ----

export const getMe = () => api<Me>("/api/me");

export const updateMe = (display_name: string) =>
  api<{ user: User }>("/api/me", { method: "PATCH", body: { display_name } });

export const changePassword = (current_password: string, new_password: string) =>
  api<{ reauthenticate: boolean; message: string }>("/api/me/password", {
    method: "POST",
    body: { current_password, new_password },
  });

export const myUsage = (range: DateRange = {}) =>
  api<UsageReport>(`/api/me/usage${qs(range)}`);

export const myMemories = () =>
  api<{ memories: Memory[] }>("/api/me/memories");

export const deleteMyMemory = (id: string) =>
  api<void>(`/api/me/memories/${encodeURIComponent(id)}`, { method: "DELETE" });

// Manual consolidation. `running` covers the scheduled pass too — it
// consolidates the caller's sessions as well, so the button must be disabled
// for it just the same. `mine` narrows that to a pass this account triggered.
export type DreamRun = {
  started_at: string;
  finished_at: string;
  episodes: number;
  added: number;
  revised: number;
  retired: number;
  purged: number;
  tokens: number;
  budget_exhausted: boolean;
  // compacted means the pass reviewed the existing store rather than learning
  // from new conversations — it distinguishes "nothing to do" from "no new
  // conversations, so we tidied what was already there".
  compacted: boolean;
  error?: string;
};

export type DreamState = {
  running: boolean;
  mine: boolean;
  last?: DreamRun;
};

export const dreamStatus = () => api<DreamState>("/api/me/dream");

export const triggerDream = () => api<DreamState>("/api/me/dream", { method: "POST" });

export const myTokens = () =>
  api<{ tokens: SessionToken[] }>("/api/me/tokens");

export const revokeToken = (id: string) =>
  api<void>(`/api/me/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });

export const revokeOtherTokens = () =>
  api<{ revoked: number }>("/api/me/tokens", { method: "DELETE" });

// ---- teams ----

export const myTeams = () => api<{ teams: Team[] }>("/api/teams");

export const createTeam = (name: string) =>
  api<{ team: Team }>("/api/teams", { method: "POST", body: { name } });

const team = (id: string) => `/api/teams/${encodeURIComponent(id)}`;

export const getTeam = (id: string) => api<{ team: Team }>(team(id));

export const renameTeam = (id: string, name: string) =>
  api<void>(team(id), { method: "PATCH", body: { name } });

export const deleteTeam = (id: string) =>
  api<void>(team(id), { method: "DELETE" });

export const listMembers = (id: string) =>
  api<{ members: Member[] }>(`${team(id)}/members`);

export const addMember = (id: string, email: string, role: TeamRole) =>
  api<{ member: Member }>(`${team(id)}/members`, {
    method: "POST",
    body: { email, role },
  });

export const changeMemberRole = (id: string, userId: string, role: TeamRole) =>
  api<void>(`${team(id)}/members/${encodeURIComponent(userId)}`, {
    method: "PATCH",
    body: { role },
  });

export const removeMember = (id: string, userId: string) =>
  api<void>(`${team(id)}/members/${encodeURIComponent(userId)}`, {
    method: "DELETE",
  });

export const listKeys = (id: string) =>
  api<{ keys: TeamKey[] }>(`${team(id)}/keys`);

export const putKey = (id: string, provider: string, api_key: string) =>
  api<{ key: TeamKey }>(`${team(id)}/keys/${encodeURIComponent(provider)}`, {
    method: "PUT",
    body: { api_key },
  });

export const deleteKey = (id: string, provider: string) =>
  api<void>(`${team(id)}/keys/${encodeURIComponent(provider)}`, {
    method: "DELETE",
  });

export const teamUsage = (id: string, range: DateRange = {}) =>
  api<UsageReport>(`${team(id)}/usage${qs(range)}`);

export const teamMemories = (id: string) =>
  api<{ memories: Memory[] }>(`${team(id)}/memories`);

export const deleteTeamMemory = (id: string, mid: string) =>
  api<void>(`${team(id)}/memories/${encodeURIComponent(mid)}`, {
    method: "DELETE",
  });

export const deprecateTeamMemory = (id: string, mid: string) =>
  api<void>(`${team(id)}/memories/${encodeURIComponent(mid)}/deprecate`, {
    method: "POST",
  });

// ---- platform ----

export type Stats = {
  users: number;
  admins: number;
  teams: number;
  usage?: Tokens;
};

export const stats = () => api<Stats>("/api/admin/stats");

export const listUsers = (params: { q?: string; limit?: number; offset?: number }) =>
  api<{ users: User[]; total: number; limit: number; offset: number }>(
    `/api/admin/users${qs(params)}`,
  );

export const createUser = (body: {
  email: string;
  password: string;
  display_name?: string;
}) => api<{ user: User }>("/api/admin/users", { method: "POST", body });

export const patchUser = (
  id: string,
  body: {
    display_name?: string;
    platform_role?: PlatformRole;
    disabled?: boolean;
  },
) =>
  api<{ user: User }>(`/api/admin/users/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body,
  });

export const resetUserPassword = (id: string, password: string) =>
  api<void>(`/api/admin/users/${encodeURIComponent(id)}/password`, {
    method: "POST",
    body: { password },
  });

export const deleteUser = (id: string) =>
  api<void>(`/api/admin/users/${encodeURIComponent(id)}`, { method: "DELETE" });

export const listAllTeams = (params: { q?: string; limit?: number; offset?: number }) =>
  api<{ teams: Team[]; total: number }>(`/api/admin/teams${qs(params)}`);

export const createTeamForOwner = (name: string, owner_user_id?: string) =>
  api<{ team: Team }>("/api/admin/teams", {
    method: "POST",
    body: { name, owner_user_id },
  });

export const platformUsage = (
  params: DateRange & { group_by?: "user" | "team"; limit?: number },
) => api<UsageReport>(`/api/admin/usage${qs(params)}`);

export const adminMemories = (params: {
  scope: "user" | "team" | "system";
  user_id?: string;
  team_id?: string;
}) => api<{ memories: Memory[] }>(`/api/admin/memories${qs(params)}`);

export const adminDeleteMemory = (id: string) =>
  api<void>(`/api/admin/memories/${encodeURIComponent(id)}`, { method: "DELETE" });

export const adminDeprecateMemory = (id: string) =>
  api<void>(`/api/admin/memories/${encodeURIComponent(id)}/deprecate`, {
    method: "POST",
  });
