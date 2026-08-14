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
import { useSyncExternalStore } from "react";
import { getSessionId } from "@/lib/thread";
import { getToken } from "@/lib/auth";
import { ApiError, handleUnauthorized } from "@/lib/api";
import { imageFileUrl } from "@/lib/image-attachment";
import { reportInteraction, approvalEpoch, type Interaction } from "@/lib/approval";
import { reportNotice } from "@/lib/notice";
import { reportPlan, type Plan } from "@/lib/plan";
import { reportPermissionMode, permissionModeFromSessionState } from "@/lib/permission";
import { t } from "@/lib/i18n";

// HISTORY_PAGE bounds the history load: the newest messages of a long
// conversation, not the whole record. The server echoes hasMore when older
// turns exist, so the UI can show a truncation hint instead of silently
// presenting a partial conversation as complete.
const HISTORY_PAGE = 100;

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
  pendingApproval?: Interaction | null;
  pendingInteractions?: Interaction[] | null;
  sessionState?: { plan?: Plan } | null;
  hasMore?: boolean;
}> {
  const threadId = getSessionId();
  if (!threadId) return { messages: [], active: false };
  const res = await fetch(
    `/api/chat/history?threadId=${encodeURIComponent(threadId)}&limit=${HISTORY_PAGE}`,
    { headers: authHeaders() },
  );
  handleUnauthorized(res);
  if (!res.ok) {
    // Follow the api<T>() convention: a non-2xx history response is a real
    // error with the server's `error` message, not an empty conversation.
    // (No history = a successful response with an empty message list.)
    const text = await res.text();
    let msg: string;
    try {
      msg =
        (JSON.parse(text) as { error?: string }).error ??
        `history load failed (${res.status})`;
    } catch {
      msg = text.slice(0, 200) || `history load failed (${res.status})`;
    }
    throw new ApiError(msg, res.status);
  }
  const data = (await res.json()) as {
    messages?: HistoryMessage[];
    active?: boolean;
    pendingApproval?: Interaction | null;
    pendingInteractions?: Interaction[] | null;
    sessionState?: { plan?: Plan } | null;
    hasMore?: boolean;
  };
  const messages = (data.messages ?? []).map((m) => mapMessage(m, threadId));
  return {
    messages,
    active: data.active === true,
    pendingApproval: data.pendingApproval ?? null,
    pendingInteractions: data.pendingInteractions ?? null,
    sessionState: data.sessionState ?? null,
    hasMore: data.hasMore === true,
  };
}

// resumeStream fetches the run's SSE from `after` and yields accumulated
// assistant-message snapshots, the ChatModelRunResult shape the runtime
// consumes when following a run. Shared by the reload path (history.resume)
// and live multi-client attach.
async function* resumeStream(
  after: number,
): AsyncGenerator<ChatModelRunResult, void, unknown> {
  const threadId = getSessionId();
  if (!threadId) return;
  // The conversation epoch this stream belongs to: if the user switches
  // session while the stream drains, its interaction frames must not land in
  // the new conversation's pending map.
  const streamEpoch = approvalEpoch();
  const res = await fetch(
    `/api/chat/resume?threadId=${encodeURIComponent(threadId)}&after=${after}`,
    { method: "POST", headers: authHeaders() },
  );
  handleUnauthorized(res);
  if (!res.ok || !res.body) return;

  const accumulated = res.body
    .pipeThrough(
      new UIMessageStreamDecoder({
        onData: (d) => {
          if (d.name === "interaction" || d.name === "tool-approval") {
            reportInteraction(d.data as Interaction, streamEpoch);
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
  // Tagged with the conversation epoch so a verdict stream still draining
  // after a session switch can't write its interactions into the new session.
  const streamEpoch = approvalEpoch();
  const accumulated = body
    .pipeThrough(
      // A verdict run can itself end on a new gate (the model asks a follow-up
      // question, or calls another client tool): surface that card live, since
      // this follow path bypasses the runtime's own onData. Mirrors App.tsx's
      // onData → reportInteraction.
      new UIMessageStreamDecoder({
        onData: (d) => {
          if (d.name === "interaction" || d.name === "tool-approval") {
            reportInteraction(d.data as Interaction, streamEpoch);
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
// hits the lightweight /active endpoint ({active: bool}, memory-first on the
// server) instead of /history, which rebuilds the whole conversation (messages
// + pending approvals + session state) on every poll — the poll runs every few
// seconds per idle tab, so the full rebuild would be constant DB pressure.
export async function hasActiveRun(): Promise<boolean> {
  const threadId = getSessionId();
  if (!threadId) return false;
  try {
    const res = await fetch(
      `/api/chat/sessions/${encodeURIComponent(threadId)}/active`,
      { headers: authHeaders() },
    );
    handleUnauthorized(res);
    if (!res.ok) return false;
    const data = (await res.json()) as { active?: boolean };
    return data.active === true;
  } catch {
    return false;
  }
}

// reportMissingSession tells the app that the backend refused an explicitly
// named thread (404 "session not found" / 403 "forbidden" on the history or
// chat paths). ChatApp listens and clears the stale local thread id, resets to
// a fresh conversation and shows a notice — a dead share link must not sit in
// a blank session that then overwrites the stored id.
export function reportMissingSession(): void {
  window.dispatchEvent(new Event("session:missing"));
}

// ---- truncation hint ----
// The last history load returned a bounded tail page (server hasMore): the
// conversation is longer than the loaded window. The flag is set on every
// load (cleared when the current conversation fits), so a fresh mount or
// session switch reflects the truth.

let truncated = false;
const truncListeners = new Set<() => void>();

function truncSubscribe(fn: () => void) {
  truncListeners.add(fn);
  return () => truncListeners.delete(fn);
}

function truncSnapshot(): boolean {
  return truncated;
}

// reportHistoryTruncated records whether the current conversation's history
// load was cut off at the page bound.
export function reportHistoryTruncated(v: boolean): void {
  if (truncated === v) return;
  truncated = v;
  for (const l of truncListeners) l();
}

// useHistoryTruncated subscribes to the truncation flag (renders the
// "only the most recent messages are shown" hint).
export function useHistoryTruncated(): boolean {
  return useSyncExternalStore(truncSubscribe, truncSnapshot);
}

// isMissingSessionError reports whether an ApiError is the backend refusing an
// explicitly named session: 404 (does not exist) and 403 (belongs to someone
// else) both mean the caller can never open it, so both clear the local id.
export function isMissingSessionError(err: unknown): err is ApiError {
  return err instanceof ApiError && (err.status === 404 || err.status === 403);
}

export const threadHistory: ThreadHistoryAdapter = {
  async load() {
    let loaded: Awaited<ReturnType<typeof loadHistory>>;
    try {
      loaded = await loadHistory();
    } catch (err) {
      // A failed history fetch (401, 500, network) must not read as "this
      // session has no history" — surface it and leave the thread empty
      // rather than pretending the conversation is blank. An explicitly
      // named session the backend refuses (404/403) is a STALE reference:
      // reportMissingSession clears it and shows the notice in ChatApp.
      if (isMissingSessionError(err)) {
        reportMissingSession();
        return ExportedMessageRepository.fromArray([]);
      }
      console.error("history load failed", err);
      reportNotice(
        err instanceof ApiError
          ? t("chat.historyLoadFailedDetail", { message: err.message })
          : t("chat.historyLoadFailed"),
      );
      return ExportedMessageRepository.fromArray([]);
    }
    const { messages, active, pendingApproval, pendingInteractions, sessionState, hasMore } = loaded;
    // A bounded load (hasMore) shows the truncation hint instead of silently
    // presenting a partial conversation as complete.
    reportHistoryTruncated(hasMore === true);
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
    // For an in-flight run the server re-streams from 0 (the server forces
    // after=0 for active runs, and a settled run has nothing to re-stream
    // broker-side), so the follow builds the full assistant message.
    return resumeStream(0);
  },

  // Server persists every event; nothing to write from the client.
  async append() {},
};
