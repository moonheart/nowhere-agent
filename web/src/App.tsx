import { useState } from "react";
import { AssistantRuntimeProvider } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { Thread } from "@/components/thread";
import { LoginForm } from "@/components/login";
import { getToken, logout } from "@/lib/auth";

export default function App() {
  const [token, setToken] = useState<string | null>(() => getToken());

  const runtime = useDataStreamRuntime({
    api: "/api/chat",
    headers: async (): Promise<Record<string, string>> =>
      token ? { authorization: `Bearer ${token}` } : {},
    onError: (e) => console.error("chat error", e),
  });

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
    <AssistantRuntimeProvider runtime={runtime}>
      <div className="flex h-dvh flex-col bg-white text-neutral-900">
        <header className="flex items-center border-b border-neutral-200 px-4 py-3">
          <span className="font-semibold">nowhere-agent</span>
          <button
            type="button"
            onClick={() => {
              void logout().finally(() => setToken(null));
            }}
            className="ml-auto text-sm text-neutral-500 hover:text-neutral-800"
          >
            Sign out
          </button>
        </header>
        <div className="min-h-0 flex-1">
          <Thread />
        </div>
      </div>
    </AssistantRuntimeProvider>
  );
}
