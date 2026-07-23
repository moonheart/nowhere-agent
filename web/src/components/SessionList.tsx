import { useCallback, useEffect, useMemo, useState } from "react";
import { MessageSquare, Plus, Search, Trash2 } from "lucide-react";
import { deleteSession, listSessions, relTime, type SessionSummary } from "@/lib/sessions";

type Props = {
  currentId: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
  // Called after the current session was deleted (so the app resets to a fresh
  // thread instead of pointing at a gone session).
  onDeleteCurrent: () => void;
  // refreshToken bumps to re-fetch (e.g. after a new session is created).
  refreshToken: number;
};

// SessionList is the left sidebar of conversations. Selecting one switches the
// active thread; "New chat" starts a fresh session; the trash icon deletes one.
export const SessionList = ({ currentId, onSelect, onNew, onDeleteCurrent, refreshToken }: Props) => {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);
  const [query, setQuery] = useState("");

  const refresh = useCallback(async () => {
    setSessions(await listSessions());
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh, refreshToken]);

  const handleDelete = async (id: string) => {
    if (!(await deleteSession(id))) return;
    if (id === currentId) {
      onDeleteCurrent();
    } else {
      void refresh();
    }
  };

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return sessions;
    return sessions.filter((s) => (s.title || "Untitled").toLowerCase().includes(q));
  }, [sessions, query]);

  return (
    <aside className="flex h-full w-64 flex-col border-r border-neutral-200 bg-neutral-50">
      <div className="space-y-2 border-b border-neutral-200 p-3">
        <button
          type="button"
          onClick={onNew}
          className="flex w-full items-center justify-center gap-2 rounded-xl bg-violet-600 px-3 py-2.5 text-sm font-medium text-white shadow-sm transition-colors hover:bg-violet-700 active:bg-violet-800"
        >
          <Plus size={16} />
          New chat
        </button>
        {sessions.length > 0 && (
          <div className="relative">
            <Search
              size={14}
              className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-neutral-400"
            />
            <input
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search chats"
              className="w-full rounded-lg border border-neutral-200 bg-white py-1.5 pl-8 pr-2 text-xs text-neutral-700 outline-none placeholder:text-neutral-400 focus:border-violet-400"
            />
          </div>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {sessions.length === 0 && (
          <p className="px-2 py-8 text-center text-xs text-neutral-400">
            No conversations yet
          </p>
        )}
        {sessions.length > 0 && filtered.length === 0 && (
          <p className="px-2 py-8 text-center text-xs text-neutral-400">
            No matches for “{query}”
          </p>
        )}
        <ul className="flex flex-col gap-0.5">
          {filtered.map((s) => {
            const active = s.id === currentId;
            return (
              <li key={s.id} className="group relative">
                <button
                  type="button"
                  onClick={() => onSelect(s.id)}
                  className={`flex w-full items-start gap-2.5 rounded-xl px-3 py-2.5 pr-9 text-left transition-colors ${
                    active
                      ? "bg-violet-100 text-violet-900"
                      : "text-neutral-700 hover:bg-neutral-200/70"
                  }`}
                >
                  <MessageSquare
                    size={15}
                    className={`mt-0.5 shrink-0 ${
                      active ? "text-violet-500" : "text-neutral-400"
                    }`}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm font-medium">
                      {s.title || "Untitled"}
                    </span>
                    <span
                      className={`block text-xs ${
                        active ? "text-violet-400" : "text-neutral-400"
                      }`}
                    >
                      {relTime(s.updatedAt)}
                    </span>
                  </span>
                </button>
                <button
                  type="button"
                  aria-label="Delete conversation"
                  title="Delete conversation"
                  onClick={(e) => {
                    e.stopPropagation();
                    void handleDelete(s.id);
                  }}
                  className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded-lg p-1.5 text-neutral-400 opacity-0 transition-opacity hover:bg-red-100 hover:text-red-600 group-hover:opacity-100"
                >
                  <Trash2 size={14} />
                </button>
              </li>
            );
          })}
        </ul>
      </div>
    </aside>
  );
};
