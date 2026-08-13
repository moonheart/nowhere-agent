// Conversation list for the sidebar: fetches the caller's sessions from the
// backend (which scopes them to the authenticated user), one page at a time.
// The server orders by most-recently-active and returns an opaque cursor to
// load the next page; an empty cursor means the list is exhausted.

import { getToken } from "@/lib/auth";

export type SessionSummary = {
  id: string;
  title: string;
  updatedAt: string;
};

// The server default page size (internal/chatapi/sessions.go); sent explicitly
// so the page size doesn't silently drift from the backend default.
export const SESSION_PAGE_SIZE = 25;

export type SessionPage = {
  sessions: SessionSummary[];
  nextCursor: string;
};

// listSessions fetches one page of the caller's conversations. q, when
// non-empty, narrows the list server-side to sessions whose title contains it
// (case-insensitive): the sidebar search runs on the backend so old
// conversations are searchable even though the client only loads 25 at a time.
// Returns null when the request FAILED — the caller must distinguish that from
// a legitimate empty list (which would otherwise render as "No conversations").
export async function listSessions(
  cursor = "",
  q = "",
): Promise<SessionPage | null> {
  const token = getToken();
  if (!token) return { sessions: [], nextCursor: "" };
  const params = new URLSearchParams({ limit: String(SESSION_PAGE_SIZE) });
  if (cursor) params.set("cursor", cursor);
  if (q) params.set("q", q);
  const res = await fetch(`/api/chat/sessions?${params}`, {
    headers: { authorization: `Bearer ${token}` },
  });
  if (!res.ok) return null;
  let data: { sessions?: SessionSummary[]; nextCursor?: string };
  try {
    data = (await res.json()) as { sessions?: SessionSummary[]; nextCursor?: string };
  } catch {
    return null;
  }
  return { sessions: data.sessions ?? [], nextCursor: data.nextCursor ?? "" };
}

// deleteSession removes a conversation; returns true on success.
export async function deleteSession(id: string): Promise<boolean> {
  const token = getToken();
  if (!token) return false;
  const res = await fetch(`/api/chat/sessions/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: { authorization: `Bearer ${token}` },
  });
  return res.ok;
}

// cancelSession stops the session's in-flight run server-side. The client's
// Stop button only aborts the local fetch; without this the model (and sandbox)
// would keep running after the stream closes.
export async function cancelSession(id: string): Promise<void> {
  const token = getToken();
  if (!token) return;
  await fetch(`/api/chat/cancel?threadId=${encodeURIComponent(id)}`, {
    method: "POST",
    headers: { authorization: `Bearer ${token}` },
  }).catch(() => {});
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
