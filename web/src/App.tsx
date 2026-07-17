import { AssistantRuntimeProvider } from "@assistant-ui/react";
import { useDataStreamRuntime } from "@assistant-ui/react-data-stream";
import { Thread } from "@/components/thread";

export default function App() {
  const runtime = useDataStreamRuntime({
    api: "/api/chat",
    onError: (e) => console.error("chat error", e),
  });

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <div className="flex h-dvh flex-col bg-white text-neutral-900">
        <header className="border-b border-neutral-200 px-4 py-3 font-semibold">
          nowhere-agent
        </header>
        <div className="min-h-0 flex-1">
          <Thread />
        </div>
      </div>
    </AssistantRuntimeProvider>
  );
}
