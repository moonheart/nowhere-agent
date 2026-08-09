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
  type ChatModelRunResult,
  type ThreadAssistantMessagePart,
  type ThreadHistoryAdapter,
  type ThreadMessageLike,
} from "@assistant-ui/react";
import {
  AssistantMessageAccumulator,
  UIMessageStreamDecoder,
} from "assistant-stream";
import { asAsyncIterableStream } from "assistant-stream/utils";
import type { ReadonlyJSONObject } from "assistant-stream/utils";
import { getSessionId } from "@/lib/thread";
import { getToken } from "@/lib/auth";
import { imageFileUrl } from "@/lib/image-attachment";
import { reportInteraction, type Interaction } from "@/lib/approval";
import { reportPlan, type Plan } from "@/lib/plan";
import { reportPermissionMode, permissionModeFromSessionState } from "@/lib/permission";

type HistoryPart =
  | { type: "text" | "reasoning"; text: string }
  | {
      type: "image";
      /** Session-relative workspace path; rendered via the /files/ endpoint. */
      mediaType?: string;
      path: string;
    }
  | {
      type: "data";
      /** Data-part name (e.g. "generative-ui" for agent-driven UI). */
      name: string;
      data: unknown;
    }
  | {
      type: "tool-call";
      toolCallId: string;
      toolName: string;
      argsText: string;
      result?: unknown;
      isError?: boolean;
      /** Nested sub-conversation (a spawn_agent child's turns). */
      messages?: HistoryMessage[];
    };
type HistoryMessage = {
  id: string;
  role: string;
  content: HistoryPart[];
  /** Server-supplied metadata (token usage as an unstable_data entry). */
  metadata?: { unstable_data?: unknown[] };
};

function authHeaders(): Record<string, string> {
  const token = getToken();
  return token ? { authorization: `Bearer ${token}` } : {};
}

// parseToolArgs best-effort parses a tool call's argsText back to an object for
// the runtime's `args` field; the raw argsText is always carried alongside, so
// an unparseable fragment still renders.
function parseToolArgs(argsText: string): ReadonlyJSONObject {
  try {
    const v = JSON.parse(argsText);
    return v && typeof v === "object" ? (v as ReadonlyJSONObject) : {};
  } catch {
    return {};
  }
}

// mapPart converts one history part to a ThreadMessageLike part, recursing into
// a tool-call's nested sub-conversation (a spawn_agent child's turns). sessionId
// resolves an image part's path to the authenticated file endpoint URL.
function mapPart(p: HistoryPart, sessionId: string): ThreadAssistantMessagePart {
  if (p.type === "reasoning") {
    return { type: "reasoning", text: p.text };
  }
  if (p.type === "image") {
    return { type: "image", image: imageFileUrl(sessionId, p.path) };
  }
  if (p.type === "data") {
    return { type: "data", name: p.name, data: p.data };
  }
  if (p.type === "tool-call") {
    return {
      type: "tool-call",
      toolCallId: p.toolCallId,
      toolName: p.toolName,
      argsText: p.argsText,
      args: parseToolArgs(p.argsText),
      result: p.result,
      isError: p.isError,
      ...(p.messages && p.messages.length > 0
        ? { messages: p.messages.map((m) => mapMessage(m, sessionId)) as never }
        : {}),
    };
  }
  return { type: "text", text: p.text };
}

function mapMessage(m: HistoryMessage, sessionId: string): ThreadMessageLike {
  return {
    id: m.id,
    role: m.role === "assistant" ? "assistant" : "user",
    content: m.content.map((p) => mapPart(p, sessionId)),
    // Carry the run's token usage (unstable_data) so a reloaded reply renders
    // the same usage footer as the live stream.
    ...(m.metadata ? { metadata: m.metadata as never } : {}),
  };
}

async function loadHistory(): Promise<{
  messages: ThreadMessageLike[];
  active: boolean;
  after: number;
  pendingApproval?: Interaction | null;
  pendingInteractions?: Interaction[] | null;
  sessionState?: { plan?: Plan } | null;
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
    pendingApproval?: Interaction | null;
    pendingInteractions?: Interaction[] | null;
    sessionState?: { plan?: Plan } | null;
  };
  const messages = (data.messages ?? []).map((m) => mapMessage(m, threadId));
  return {
    messages,
    active: data.active === true,
    after: typeof data.after === "number" ? data.after : 0,
    pendingApproval: data.pendingApproval ?? null,
    pendingInteractions: data.pendingInteractions ?? null,
    sessionState: data.sessionState ?? null,
  };
}

// lastLoadedAfter is the run-event offset the most recent load() snapshot
// already covered. resume() passes it to the server so a reconnect streams only
// events that arrived after the snapshot — resuming from 0 would replay the
// whole run and duplicate the assistant reply.
let lastLoadedAfter = 0;

// resumeStream fetches the run's SSE from `after` and yields accumulated
// assistant-message snapshots, the ChatModelRunResult shape the runtime
// consumes when following a run. Shared by the reload path (history.resume)
// and live multi-client attach.
async function* resumeStream(
  after: number,
): AsyncGenerator<ChatModelRunResult, void, unknown> {
  const threadId = getSessionId();
  if (!threadId) return;
  const res = await fetch(
    `/api/chat/resume?threadId=${encodeURIComponent(threadId)}&after=${after}`,
    { method: "POST", headers: authHeaders() },
  );
  if (!res.ok || !res.body) return;

  const accumulated = res.body
    .pipeThrough(
      new UIMessageStreamDecoder({
        onData: (d) => {
          if (d.name === "interaction" || d.name === "tool-approval") {
            reportInteraction(d.data as Interaction);
          }
        },
      }),
    )
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
}

// attachStream re-streams the session's in-flight run from the beginning, for a
// client attaching to a run started elsewhere (another tab/device). Exporting
// it lets App.tsx hand it to threadRuntime.resumeRun() — the runtime aborts any
// prior stream for us, so overlapping attaches dedup themselves.
export function attachStream(): ReturnType<typeof resumeStream> {
  return resumeStream(0);
}

// followBody decodes an already-open ui-message-stream body (e.g. the response
// to an approval verdict, which streams the resumed run's continuation) into
// the accumulated-message snapshots the runtime consumes. Same pipeline as
// resumeStream; only the source (an open body vs. a fresh fetch) differs.
export async function* followBody(
  body: ReadableStream<Uint8Array>,
): AsyncGenerator<ChatModelRunResult, void, unknown> {
  const accumulated = body
    .pipeThrough(
      // A verdict run can itself end on a new gate (the model asks a follow-up
      // question, or calls another client tool): surface that card live, since
      // this follow path bypasses the runtime's own onData. Mirrors App.tsx's
      // onData → reportInteraction.
      new UIMessageStreamDecoder({
        onData: (d) => {
          if (d.name === "interaction" || d.name === "tool-approval") {
            reportInteraction(d.data as Interaction);
          }
        },
      }),
    )
    .pipeThrough(new AssistantMessageAccumulator());
  for await (const message of asAsyncIterableStream(accumulated)) {
    yield {
      content: message.parts as unknown as ThreadAssistantMessagePart[],
      status: message.status,
    };
  }
}

// hasActiveRun reports whether the session's run is still in flight. Used by
// the idle-poll that lets a second client notice a run started elsewhere. It
// reads only the `active` flag, skipping loadHistory()'s message rebuild and
// offset scan so the frequent poll stays cheap.
export async function hasActiveRun(): Promise<boolean> {
  const threadId = getSessionId();
  if (!threadId) return false;
  try {
    const res = await fetch(
      `/api/chat/history?threadId=${encodeURIComponent(threadId)}`,
      { headers: authHeaders() },
    );
    if (!res.ok) return false;
    const data = (await res.json()) as { active?: boolean };
    return data.active === true;
  } catch {
    return false;
  }
}

export const threadHistory: ThreadHistoryAdapter = {
  async load() {
    const { messages, active, after, pendingApproval, pendingInteractions, sessionState } = await loadHistory();
    lastLoadedAfter = after;
    // Re-show every parked interaction of a gated batch (the transient frames
    // dropped on refresh); the durable rows are the source of truth, echoed by
    // /history as pendingInteractions (queue order). Fall back to the singular
    // pendingApproval for older backends.
    const pending = pendingInteractions && pendingInteractions.length > 0
      ? pendingInteractions
      : pendingApproval
        ? [pendingApproval]
        : [];
    for (const p of pending) reportInteraction(p);
    // Restore the plan panel (capability-gap O1): the session's persisted plan
    // state is echoed as sessionState.plan, the same source the live
    // data-session-state frames feed.
    if (sessionState?.plan) reportPlan(sessionState.plan);
    // Restore the permission mode (per-session allow-all toggle): echoed as
    // sessionState.permission_mode, the same source the live frames feed.
    const st = sessionState as Record<string, unknown> | null | undefined;
    const mode = permissionModeFromSessionState({ key: "permission_mode", value: st?.permission_mode });
    if (mode !== null) reportPermissionMode(mode);
    // When a run is in flight, drop the trailing partial assistant message from
    // the snapshot. The follow (resume) re-streams the whole run and renders
    // that message itself; importing a partial assistant message AND following
    // would split the run into two bubbles — the runtime's resume always starts
    // a NEW assistant message rather than extending the imported one. The
    // server also re-streams an active run from offset 0 regardless of `after`.
    const imported =
      active && messages.length > 0 && messages.at(-1)?.role === "assistant"
        ? messages.slice(0, -1)
        : messages;
    // unstable_resume asks the runtime to call resume() after import — but the
    // runtime starts a NEW assistant message at the head for it. Only opt in
    // when a run is genuinely still in flight; resuming a completed run would
    // duplicate the assistant reply that load() already restored.
    return {
      ...ExportedMessageRepository.fromArray(imported),
      unstable_resume: active,
    };
  },

  resume() {
    // For an in-flight run the server re-streams from 0, so the follow builds
    // the full assistant message. (lastLoadedAfter only bounds a settled run.)
    return resumeStream(lastLoadedAfter);
  },

  // Server persists every event; nothing to write from the client.
  async append() {},
};
