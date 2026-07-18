// Conversation list for the sidebar: fetches the caller's sessions from the
// backend (which scopes them to the authenticated user).

import { getToken } from "@/lib/auth";

export type SessionSummary = {
  id: string;
  title: string;
  updatedAt: string;
};

export async function listSessions(): Promise<SessionSummary[]> {
  const token = getToken();
  if (!token) return [];
  const res = await fetch("/api/chat/sessions", {
    headers: { authorization: `Bearer ${token}` },
  });
  if (!res.ok) return [];
  const data = (await res.json()) as { sessions?: SessionSummary[] };
  return data.sessions ?? [];
}

// relTime renders a compact relative timestamp for the sidebar.
export function relTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const s = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (s < 60) return "just now";
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 7) return `${d}d ago`;
  return new Date(then).toLocaleDateString();
}
