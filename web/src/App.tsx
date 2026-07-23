import { useEffect, useState } from "react";
import { AssistantRuntimeProvider } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { LogOut } from "lucide-react";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { SessionList } from "@/components/SessionList";
import { RightPanel } from "@/components/right-panel";
import { getToken, logout } from "@/lib/auth";
import { getSessionId, setSessionId, clearSessionId } from "@/lib/thread";
import { threadHistory, attachStream, hasActiveRun } from "@/lib/history";
import { resetActivity, reportSubagentActivity, type SubagentSignal } from "@/lib/activity";
import { cancelSession } from "@/lib/sessions";

// Chat holds one conversation: remounting it (via React key) resets the runtime
// and re-runs history.load() for the now-current sessionId.
function Chat({
  conversationKey,
  onSession,
}: {
  conversationKey: number;
  onSession: (id: string) => void;
}) {
  const runtime = useDataStreamRuntime({
    api: "/api/chat",
    headers: async (): Promise<Record<string, string>> => {
      const token = getToken();
      return token ? { authorization: `Bearer ${token}` } : {};
    },
    body: async () => {
      const threadId = getSessionId();
      return threadId ? { threadId } : {};
    },
    onData: (d) => {
      if (d.name === "session") {
        const id = (d.data as { id?: string })?.id;
        if (id) {
          setSessionId(id);
          onSession(id);
        }
      } else if (d.name === "subagent") {
        // Live subagent progress (from a spawn_agent tool call) — feed the
        // right panel's Runs tab; transient, never part of the message.
        reportSubagentActivity(d.data as SubagentSignal);
      }
    },
    // The Stop button only aborts the local fetch; also tell the backend to
    // cancel the run so the model + sandbox stop, not just the HTTP stream.
    onCancel: () => {
      const id = getSessionId();
      if (id) void cancelSession(id);
    },
    adapters: { history: threadHistory },
    onError: (e) => console.error("chat error", e),
  });

  // Multi-client attach (design D13): while this client is idle, poll the
  // session to notice a run started on another tab/device and live-follow it
  // via resumeRun. The follow re-streams the whole run and renders ONE new
  // assistant message; the runtime aborts any prior stream before a new one, so
  // a poll attach racing the load+resume follow just re-follows (aborting the
  // stale stream) rather than duplicating — the resume stream replaces the
  // same new message's content each time.
  useEffect(() => {
    let cancelled = false;
    let attaching = false;
    const tick = async () => {
      if (cancelled || attaching) return;
      const threadId = getSessionId();
      if (!threadId) return;
      if (runtime.thread.getState().isRunning) return;
      let active = false;
      try {
        active = await hasActiveRun();
      } catch {
        return; // transient failure; try again next tick
      }
      if (cancelled || !active || runtime.thread.getState().isRunning) return;
      attaching = true;
      try {
        // parentId: null → startRun appends the streamed run after the current
        // head, which is the user message this run is answering.
        await runtime.thread.resumeRun({ parentId: null, stream: attachStream });
      } catch {
        // Attach is best-effort; a failed follow is retried next tick.
      } finally {
        attaching = false;
      }
    };
    const id = setInterval(tick, 2000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [runtime]);

  return (
    <AssistantRuntimeProvider runtime={runtime} key={conversationKey}>
      <Thread />
    </AssistantRuntimeProvider>
  );
}

export default function App() {
  const [token, setToken] = useState<string | null>(() => getToken());
  const [conversationKey, setConversationKey] = useState(0);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(() =>
    getSessionId(),
  );
  // Bumped whenever a session is created/switched so the sidebar refetches.
  const [listVersion, setListVersion] = useState(0);

  const startNewChat = () => {
    clearSessionId();
    setActiveSessionId(null);
    resetActivity();
    setConversationKey((k) => k + 1);
  };

  // Deleting the active session resets to a fresh thread and refreshes the list.
  const handleDeleteCurrent = () => {
    startNewChat();
    setListVersion((v) => v + 1);
  };

  const switchTo = (id: string) => {
    if (id === activeSessionId) return;
    setSessionId(id);
    setActiveSessionId(id);
    resetActivity();
    setConversationKey((k) => k + 1);
  };

  // Called when a brand-new session is created server-side (first message of a
  // new chat): adopt it as active and refresh the sidebar. The runtime is
  // already streaming into this session, so no remount is needed.
  const handleNewSession = (id: string) => {
    setActiveSessionId((prev) => {
      if (prev === id) return prev;
      setListVersion((v) => v + 1);
      return id;
    });
  };

  if (!token) {
    return (
      <div className="flex h-dvh flex-col bg-neutral-50 text-neutral-900">
        <div className="min-h-0 flex-1">
          <LoginForm onSuccess={() => setToken(getToken())} />
        </div>
      </div>
    );
  }

  return (
    <div className="flex h-dvh flex-col bg-neutral-50 text-neutral-900">
      <header className="flex h-12 items-center gap-3 border-b border-neutral-200 bg-white px-4">
        <div className="flex items-center gap-2">
          <span className="flex h-6 w-6 items-center justify-center rounded-lg bg-violet-600 text-xs font-bold text-white">
            n
          </span>
          <span className="text-sm font-semibold tracking-tight">
            nowhere-agent
          </span>
        </div>
        <div className="ml-auto flex items-center gap-1">
          <button
            type="button"
            title="Sign out"
            onClick={() => {
              clearSessionId();
              void logout().finally(() => setToken(null));
            }}
            className="flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-sm text-neutral-500 transition-colors hover:bg-neutral-100 hover:text-neutral-800"
          >
            <LogOut size={15} />
            Sign out
          </button>
        </div>
      </header>
      <div className="flex min-h-0 flex-1">
        <SessionList
          currentId={activeSessionId}
          onSelect={switchTo}
          onNew={startNewChat}
          onDeleteCurrent={handleDeleteCurrent}
          refreshToken={listVersion}
        />
        <main className="min-w-0 flex-1 bg-white">
          <Chat conversationKey={conversationKey} onSession={handleNewSession} />
        </main>
        <RightPanel />
      </div>
    </div>
  );
}
