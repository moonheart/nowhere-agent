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
  // Bound mobile number, masked server-side ("****8000"); empty when unbound.
  phone?: string;
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

// ---- provider registry (change provider-registry) ----

export type Provider = {
  id: string;
  scope: "system" | "team";
  team_id?: string;
  name: string;
  vendor: "anthropic" | "openai";
  base_url?: string;
  // Masked server-side; the plaintext key is write-only.
  key: string;
  is_default: boolean;
  enabled: boolean;
  models?: ProviderModel[];
  created_at: string;
  updated_at: string;
};

export type ProviderModel = {
  id: string;
  provider_id: string;
  name: string;
  display_name?: string;
  vision: boolean;
  context_window?: number | null;
  is_default: boolean;
  enabled: boolean;
  created_at: string;
  updated_at: string;
};

export type ProviderAssignment = {
  provider_id: string;
  model_id?: string;
};

export type ProviderBody = {
  name?: string;
  vendor?: "anthropic" | "openai";
  base_url?: string;
  api_key?: string;
  enabled?: boolean;
};

export type ProviderModelBody = {
  name?: string;
  display_name?: string;
  vision?: boolean;
  context_window?: number | null;
  clear_context_window?: boolean;
  enabled?: boolean;
};

// FetchedModel is one model returned by the "fetch models" action: its name on
// the provider's API and whether the registry already holds it. Fetching is a
// preview — the user picks which names to register.
export type FetchedModel = { name: string; registered: boolean };

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
  group_by?: "user" | "team" | "model";
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

// bindPhone OTP-verifies and binds the caller's mobile number, enabling
// phone-based password recovery. The code is issued by requestPhoneCode.
export const bindPhone = (phone: string, code: string) =>
  api<{ message: string }>("/api/me/phone/bind", {
    method: "POST",
    body: { phone, code },
  });

// deleteMeAccount removes the caller's own account and its data (PIPL §47
// erasure right). The server revokes every token with the account.
export const deleteMeAccount = () =>
  api<void>("/api/me", { method: "DELETE" });

// ---- second factor (TOTP/MFA) ----

// enableTotp starts enrollment: the secret and otpauth URI are returned once
// and the factor only activates after confirmTotp validates a code.
export const enableTotp = () =>
  api<{ secret: string; uri: string }>("/api/me/totp/enable", {
    method: "POST",
  });

export const confirmTotp = (code: string) =>
  api<void>("/api/me/totp/confirm", { method: "POST", body: { code } });

export const disableTotp = (code: string) =>
  api<void>("/api/me/totp/disable", { method: "POST", body: { code } });

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

// Teams configure their own providers AND may use system providers; the
// assignment picks which provider+model serves the team's runs.
export type TeamProviderListing = {
  providers: Provider[];
  assignment: ProviderAssignment | null;
};

export const listTeamProviders = (id: string) =>
  api<TeamProviderListing>(`${team(id)}/providers`);

export const createTeamProvider = (id: string, body: ProviderBody) =>
  api<{ provider: Provider }>(`${team(id)}/providers`, {
    method: "POST",
    body,
  });

export const updateTeamProvider = (id: string, pid: string, body: ProviderBody) =>
  api<{ provider: Provider }>(`${team(id)}/providers/${encodeURIComponent(pid)}`, {
    method: "PATCH",
    body,
  });

export const deleteTeamProvider = (id: string, pid: string) =>
  api<void>(`${team(id)}/providers/${encodeURIComponent(pid)}`, {
    method: "DELETE",
  });

export const createTeamModel = (id: string, pid: string, body: ProviderModelBody) =>
  api<{ model: ProviderModel }>(
    `${team(id)}/providers/${encodeURIComponent(pid)}/models`,
    { method: "POST", body },
  );

export const updateTeamModel = (
  id: string,
  pid: string,
  mid: string,
  body: ProviderModelBody,
) =>
  api<{ model: ProviderModel }>(
    `${team(id)}/providers/${encodeURIComponent(pid)}/models/${encodeURIComponent(mid)}`,
    { method: "PATCH", body },
  );

export const deleteTeamModel = (id: string, pid: string, mid: string) =>
  api<void>(
    `${team(id)}/providers/${encodeURIComponent(pid)}/models/${encodeURIComponent(mid)}`,
    { method: "DELETE" },
  );

export const setTeamDefaultModel = (id: string, pid: string, mid: string) =>
  api<void>(
    `${team(id)}/providers/${encodeURIComponent(pid)}/models/${encodeURIComponent(mid)}/default`,
    { method: "POST" },
  );

// Fetches the provider's model list from its own API as a preview; nothing is
// registered until the caller adds the models it selects.
export const fetchTeamModels = (id: string, pid: string) =>
  api<{ models: FetchedModel[] }>(
    `${team(id)}/providers/${encodeURIComponent(pid)}/models/fetch`,
    { method: "POST" },
  );

export const setTeamAssignment = (id: string, assignment: ProviderAssignment) =>
  api<{ assignment: ProviderAssignment }>(`${team(id)}/provider-assignment`, {
    method: "PUT",
    body: assignment,
  });

export const clearTeamAssignment = (id: string) =>
  api<void>(`${team(id)}/provider-assignment`, { method: "DELETE" });

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

export const deleteSession = (id: string) =>
  api<void>(`/api/admin/sessions/${encodeURIComponent(id)}`, { method: "DELETE" });

export const listAllTeams = (params: { q?: string; limit?: number; offset?: number }) =>
  api<{ teams: Team[]; total: number }>(`/api/admin/teams${qs(params)}`);

export const createTeamForOwner = (name: string, owner_user_id?: string) =>
  api<{ team: Team }>("/api/admin/teams", {
    method: "POST",
    body: { name, owner_user_id },
  });

export const platformUsage = (
  params: DateRange & { group_by?: "user" | "team" | "model"; limit?: number },
) => api<UsageReport>(`/api/admin/usage${qs(params)}`);

// System providers are platform-managed; one of them is the platform default
// every team without an assignment falls back to.
const adminProvider = (id: string) =>
  `/api/admin/providers/${encodeURIComponent(id)}`;

export const listSystemProviders = () =>
  api<{ providers: Provider[] }>("/api/admin/providers");

export const createSystemProvider = (body: ProviderBody) =>
  api<{ provider: Provider }>("/api/admin/providers", { method: "POST", body });

export const updateSystemProvider = (id: string, body: ProviderBody) =>
  api<{ provider: Provider }>(adminProvider(id), { method: "PATCH", body });

export const deleteSystemProvider = (id: string) =>
  api<void>(adminProvider(id), { method: "DELETE" });

export const setSystemDefaultProvider = (id: string) =>
  api<void>(`${adminProvider(id)}/default`, { method: "POST" });

export const createSystemModel = (id: string, body: ProviderModelBody) =>
  api<{ model: ProviderModel }>(`${adminProvider(id)}/models`, {
    method: "POST",
    body,
  });

export const updateSystemModel = (
  id: string,
  mid: string,
  body: ProviderModelBody,
) =>
  api<{ model: ProviderModel }>(
    `${adminProvider(id)}/models/${encodeURIComponent(mid)}`,
    { method: "PATCH", body },
  );

export const deleteSystemModel = (id: string, mid: string) =>
  api<void>(`${adminProvider(id)}/models/${encodeURIComponent(mid)}`, {
    method: "DELETE",
  });

export const setSystemDefaultModel = (id: string, mid: string) =>
  api<void>(
    `${adminProvider(id)}/models/${encodeURIComponent(mid)}/default`,
    { method: "POST" },
  );

// Fetches the provider's model list from its own API as a preview; nothing is
// registered until the caller adds the models it selects.
export const fetchSystemModels = (id: string) =>
  api<{ models: FetchedModel[] }>(`${adminProvider(id)}/models/fetch`, {
    method: "POST",
  });

// ---- quota configuration ----

// QuotaBudget mirrors the server's quota budget DTO: one scope's monthly token
// cap. scope "user" caps one account, "team" caps the spend billed to one
// team's provider key.
export type QuotaBudget = {
  scope: "user" | "team";
  owner_id: string;
  monthly_tokens: number;
  updated_at: string;
};

// getQuota reads one scope's budget. budget is null when none is set — the
// "no limit" state, which the server answers as 200/null rather than 404.
export const getQuota = (scope: "user" | "team", owner_id: string) =>
  api<{ budget: QuotaBudget | null }>(
    `/api/admin/quotas${qs({ scope, owner_id })}`,
  );

export const putQuota = (body: {
  scope: "user" | "team";
  owner_id: string;
  monthly_tokens: number;
}) => api<{ budget: QuotaBudget }>("/api/admin/quotas", { method: "PUT", body });

export const clearQuota = (scope: "user" | "team", owner_id: string) =>
  api<void>(`/api/admin/quotas${qs({ scope, owner_id })}`, { method: "DELETE" });

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

// ---- audit trail ----

// AuditEntry mirrors the server's audit.Entry. actor_email is a snapshot kept
// even after the account is deleted, so it is the display name; actor_id may be
// empty for events with no authenticated actor (a failed login names no one).
export type AuditEntry = {
  id: number;
  created_at: string;
  actor_id?: string;
  actor_email?: string;
  action: string;
  outcome: string;
  target_type?: string;
  target_id?: string;
  ip?: string;
  ua?: string;
  detail?: unknown;
};

// The audit filters pass dates as YYYY-MM-DD, which the server parses as a day
// boundary; DateRange (from/to) is the shared shape the range pickers produce.
export type AuditQuery = {
  action?: string;
  actor?: string;
  from?: string;
  to?: string;
  limit?: number;
  offset?: number;
};

// The action set is fixed server-side; listing it here lets the filter offer a
// dropdown of meaningful values instead of a free-text field nobody can guess.
export const AUDIT_ACTIONS = [
  "auth.signup",
  "auth.login",
  "auth.logout",
  "me.password.change",
  "me.token.revoke",
  "admin.user.create",
  "admin.user.update",
  "admin.user.disable",
  "admin.user.enable",
  "admin.user.reset_password",
  "admin.user.delete",
  "admin.user.set_role",
  "team.create",
  "team.rename",
  "team.delete",
  "team.member.add",
  "team.member.remove",
  "team.member.set_role",
  "team.key.set",
  "team.key.delete",
  "provider.create",
  "provider.update",
  "provider.delete",
  "provider.set_default",
  "provider.model.create",
  "provider.model.update",
  "provider.model.delete",
  "provider.model.set_default",
  "team.provider.assign",
  "team.provider.assign.clear",
  "quota.set",
  "quota.clear",
  "memory.delete",
  "memory.deprecate",
] as const;

export const listAudit = (params: AuditQuery) =>
  api<{ entries: AuditEntry[]; total: number; limit: number; offset: number }>(
    `/api/admin/audit${qs(params)}`,
  );
