import { useState } from "react";
import { AssistantRuntimeProvider } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { getToken, logout } from "@/lib/auth";
import { getSessionId, setSessionId, clearSessionId } from "@/lib/thread";
import { threadHistory } from "@/lib/history";

// Chat holds one conversation: remounting it (via React key) resets the runtime
// to a fresh thread, which is how "New chat" starts a new session.
function Chat({ conversationKey }: { conversationKey: number }) {
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
        if (id) setSessionId(id);
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

  const startNewChat = () => {
    clearSessionId();
    setConversationKey((k) => k + 1);
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
          onClick={startNewChat}
          className="ml-4 rounded-lg border border-neutral-300 px-3 py-1 text-sm text-neutral-600 hover:bg-neutral-100"
        >
          New chat
        </button>
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
      <div className="min-h-0 flex-1">
        <Chat conversationKey={conversationKey} />
      </div>
    </div>
  );
}
