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

async function loadMessages(): Promise<ThreadMessageLike[]> {
  const threadId = getSessionId();
  if (!threadId) return [];
  const res = await fetch(
    `/api/chat/history?threadId=${encodeURIComponent(threadId)}`,
    { headers: authHeaders() },
  );
  if (!res.ok) return [];
  const data = (await res.json()) as { messages?: HistoryMessage[] };
  return (data.messages ?? []).map(
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
}

export const threadHistory: ThreadHistoryAdapter = {
  async load() {
    const messages = await loadMessages();
    // unstable_resume asks the runtime to call resume() after import, so a
    // reloaded page re-streams the session's run instead of sitting static.
    return { ...ExportedMessageRepository.fromArray(messages), unstable_resume: true };
  },

  async *resume() {
    const threadId = getSessionId();
    if (!threadId) return;
    const res = await fetch(
      `/api/chat/resume?threadId=${encodeURIComponent(threadId)}&after=0`,
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
