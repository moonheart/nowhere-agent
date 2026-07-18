// ThreadHistoryAdapter backed by the nowhere-agent session run log: load()
// rebuilds prior messages from run_events (replay), resume() re-streams an
// in-progress run so a reconnecting client catches up. Persistence itself
// happens server-side, so append() is a no-op.
//
// NOTE: `assistant-stream` is a direct dependency pinned to the exact version
// @assistant-ui/react-data-stream uses internally. We decode resume streams
// with the same UIMessageStreamDecoder/AssistantMessageAccumulator pipeline;
// keeping one copy avoids two divergent stream implementations in the bundle.
// When bumping react-data-stream, bump this to match its assistant-stream.

import {
  ExportedMessageRepository,
  type ThreadAssistantMessagePart,
  type ThreadHistoryAdapter,
  type ThreadMessageLike,
} from "@assistant-ui/react";
import {
  AssistantMessageAccumulator,
  UIMessageStreamDecoder,
} from "assistant-stream";
import { asAsyncIterableStream } from "assistant-stream/utils";
import { getSessionId } from "@/lib/thread";
import { getToken } from "@/lib/auth";

type HistoryPart = { type: "text" | "reasoning"; text: string };
type HistoryMessage = { id: string; role: string; content: HistoryPart[] };

function authHeaders(): Record<string, string> {
  const token = getToken();
  return token ? { authorization: `Bearer ${token}` } : {};
}

async function loadHistory(): Promise<{
  messages: ThreadMessageLike[];
  active: boolean;
  after: number;
}> {
  const threadId = getSessionId();
  if (!threadId) return { messages: [], active: false, after: 0 };
  const res = await fetch(
    `/api/chat/history?threadId=${encodeURIComponent(threadId)}`,
    { headers: authHeaders() },
  );
  if (!res.ok) return { messages: [], active: false, after: 0 };
  const data = (await res.json()) as {
    messages?: HistoryMessage[];
    active?: boolean;
    after?: number;
  };
  const messages = (data.messages ?? []).map(
    (m): ThreadMessageLike => ({
      id: m.id,
      role: m.role === "assistant" ? "assistant" : "user",
      content: m.content.map((p) =>
        p.type === "reasoning"
          ? { type: "reasoning", text: p.text }
          : { type: "text", text: p.text },
      ),
    }),
  );
  return {
    messages,
    active: data.active === true,
    after: typeof data.after === "number" ? data.after : 0,
  };
}

// lastLoadedAfter is the run-event offset the most recent load() snapshot
// already covered. resume() passes it to the server so a reconnect streams only
// events that arrived after the snapshot — resuming from 0 would replay the
// whole run and duplicate the assistant reply.
let lastLoadedAfter = 0;

export const threadHistory: ThreadHistoryAdapter = {
  async load() {
    const { messages, active, after } = await loadHistory();
    lastLoadedAfter = after;
    // unstable_resume asks the runtime to call resume() after import — but the
    // runtime starts a NEW assistant message at the head for it. Only opt in
    // when a run is genuinely still in flight; resuming a completed run would
    // duplicate the assistant reply that load() already restored.
    return {
      ...ExportedMessageRepository.fromArray(messages),
      unstable_resume: active,
    };
  },

  async *resume() {
    const threadId = getSessionId();
    if (!threadId) return;
    const res = await fetch(
      `/api/chat/resume?threadId=${encodeURIComponent(threadId)}&after=${lastLoadedAfter}`,
      { method: "POST", headers: authHeaders() },
    );
    if (!res.ok || !res.body) return;

    const accumulated = res.body
      .pipeThrough(new UIMessageStreamDecoder())
      .pipeThrough(new AssistantMessageAccumulator());
    // The accumulator's `parts` is the full accumulated content at each chunk
    // (text deltas extend the trailing part in place), so we yield every
    // snapshot; the runtime replaces content each time, growing the message.
    for await (const message of asAsyncIterableStream(accumulated)) {
      yield {
        content: message.parts as unknown as ThreadAssistantMessagePart[],
        status: message.status,
      };
    }
  },

  // Server persists every event; nothing to write from the client.
  async append() {},
};
