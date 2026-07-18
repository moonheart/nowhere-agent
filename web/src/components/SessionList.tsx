import { useCallback, useEffect, useState } from "react";
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
// active thread; "New chat" starts a fresh session; the × deletes one.
export const SessionList = ({ currentId, onSelect, onNew, onDeleteCurrent, refreshToken }: Props) => {
  const [sessions, setSessions] = useState<SessionSummary[]>([]);

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

  return (
    <aside className="flex h-full w-64 flex-col border-r border-neutral-200 bg-neutral-50">
      <div className="border-b border-neutral-200 p-3">
        <button
          type="button"
          onClick={onNew}
          className="w-full rounded-lg bg-violet-600 px-3 py-2 text-sm font-medium text-white hover:bg-violet-700"
        >
          + New chat
        </button>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-2">
        {sessions.length === 0 && (
          <p className="px-2 py-4 text-center text-xs text-neutral-400">
            No conversations yet
          </p>
        )}
        <ul className="flex flex-col gap-0.5">
          {sessions.map((s) => {
            const active = s.id === currentId;
            return (
              <li key={s.id} className="group relative">
                <button
                  type="button"
                  onClick={() => onSelect(s.id)}
                  className={`w-full rounded-lg px-3 py-2 pr-8 text-left text-sm transition-colors ${
                    active
                      ? "bg-violet-100 text-violet-900"
                      : "text-neutral-700 hover:bg-neutral-200"
                  }`}
                >
                  <span className="block truncate font-medium">
                    {s.title || "Untitled"}
                  </span>
                  <span className="block text-xs text-neutral-400">
                    {relTime(s.updatedAt)}
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
                  className="absolute right-1.5 top-1/2 -translate-y-1/2 rounded p-1 text-neutral-400 opacity-0 transition-opacity hover:bg-neutral-300 hover:text-neutral-700 group-hover:opacity-100"
                >
                  <svg width="14" height="14" viewBox="0 0 14 14" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round">
                    <path d="M3 3l8 8M11 3l-8 8" />
                  </svg>
                </button>
              </li>
            );
          })}
        </ul>
      </div>
    </aside>
  );
};
