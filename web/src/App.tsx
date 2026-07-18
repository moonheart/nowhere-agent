import { useState } from "react";
import { AssistantRuntimeProvider } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { SessionList } from "@/components/SessionList";
import { getToken, logout } from "@/lib/auth";
import { getSessionId, setSessionId, clearSessionId } from "@/lib/thread";
import { threadHistory } from "@/lib/history";

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
      }
    },
    adapters: { history: threadHistory },
    onError: (e) => console.error("chat error", e),
  });

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
    setConversationKey((k) => k + 1);
  };

  const switchTo = (id: string) => {
    if (id === activeSessionId) return;
    setSessionId(id);
    setActiveSessionId(id);
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
      <div className="flex h-dvh flex-col bg-white text-neutral-900">
        <header className="border-b border-neutral-200 px-4 py-3 font-semibold">
          nowhere-agent
        </header>
        <div className="min-h-0 flex-1">
          <LoginForm onSuccess={() => setToken(getToken())} />
        </div>
      </div>
    );
  }

  return (
    <div className="flex h-dvh flex-col bg-white text-neutral-900">
      <header className="flex items-center border-b border-neutral-200 px-4 py-3">
        <span className="font-semibold">nowhere-agent</span>
        <button
          type="button"
          onClick={() => {
            clearSessionId();
            void logout().finally(() => setToken(null));
          }}
          className="ml-auto text-sm text-neutral-500 hover:text-neutral-800"
        >
          Sign out
        </button>
      </header>
      <div className="flex min-h-0 flex-1">
        <SessionList
          currentId={activeSessionId}
          onSelect={switchTo}
          onNew={startNewChat}
          refreshToken={listVersion}
        />
        <div className="min-w-0 flex-1">
          <Chat conversationKey={conversationKey} onSession={handleNewSession} />
        </div>
      </div>
    </div>
  );
}
