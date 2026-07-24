import { useEffect, useRef, useState, type FC } from "react";
import type { ThreadMessage, ToolCallMessagePartProps } from "@assistant-ui/react";
import { Bot } from "lucide-react";
import { reportToolCall, useActivity, type SubPart } from "@/lib/activity";
import { Reasoning } from "@/components/reasoning";
import { MarkdownText } from "@/components/markdown-text";

/**
 * Renders a tool call (file read/write, etc.) as a collapsible block in the
 * assistant message. The header always shows the tool name and live status;
 * expanding it reveals the arguments the model sent and the result the tool
 * returned. Errors are highlighted.
 *
 * A spawn_agent call is the recursive case: its body renders the child's own
 * parts (thinking / text / tool calls) with the SAME components a top-level
 * assistant message uses, so a subagent's output — including a nested
 * spawn_agent — looks exactly like the parent's.
 */
export const ToolCall: FC<ToolCallMessagePartProps> = (props) => {
  if (props.toolName === "spawn_agent") {
    return <SubagentCall {...props} />;
  }
  return <GenericCall {...props} />;
};

// dispatch routes any tool call to its renderer; GenericCall uses it so a
// subagent's nested spawn_agent recurses into SubagentCall.
const dispatch: FC<ToolCallMessagePartProps> = (props) =>
  props.toolName === "spawn_agent" ? <SubagentCall {...props} /> : <GenericCall {...props} />;

/* ---------- spawn_agent (recursive) ---------- */

const SubagentCall: FC<ToolCallMessagePartProps> = (props) => {
  const { toolName, result, isError, status, toolCallId } = props;
  const running = status?.type === "running";

  // Auto-collapse: open while running, snap shut the moment it completes. The
  // user can still re-open manually afterwards.
  const [open, setOpen] = useState(true);
  const wasRunning = useRef(running);
  useEffect(() => {
    if (running) {
      wasRunning.current = true;
      setOpen(true);
    } else if (wasRunning.current) {
      wasRunning.current = false;
      setOpen(false);
    }
  }, [running]);

  // Find the matching live subagent run by tool-call id; its parts stream in.
  const { subagents } = useActivity();
  const live = subagents.find((s) => s.toolCallId && s.toolCallId === toolCallId);

  useReport(props, running);

  const resultText = toText(result);
  const liveParts = live?.parts ?? [];
  const replayMessages = (props.messages as readonly ThreadMessage[] | undefined) ?? [];
  // Prefer the live stream; on reload (no live run) replay the persisted
  // sub-conversation; else fall back to the collapsed result text.
  const mode = liveParts.length > 0 ? "live" : replayMessages.length > 0 ? "replay" : "result";

  return (
    <div
      className={`mb-2 rounded-xl border text-sm ${
        isError ? "border-red-200 bg-red-50" : "border-violet-200 bg-violet-50/40"
      }`}
    >
      <Header
        icon={<Bot size={13} className="shrink-0 text-violet-500" />}
        name={toolName}
        running={running}
        isError={isError}
        expanded={open}
        onToggle={() => setOpen((o) => !o)}
        badge={live && live.depth > 1 ? `L${live.depth}` : undefined}
      />
      {open && (
        <div className="max-h-96 space-y-2 overflow-y-auto border-t border-violet-100 px-3 py-2">
          {mode === "live" && (
            <>
              <SubParts parts={liveParts} running={running} />
              {running && <span className="animate-pulse text-violet-400">▍</span>}
            </>
          )}
          {mode === "replay" && <NestedReplay messages={replayMessages} />}
          {mode === "result" && (
            <>
              {running && <div className="text-xs text-neutral-400">subagent working…</div>}
              {resultText && (
                <div className={isError ? "font-mono text-xs text-red-600" : ""}>
                  {isError ? (
                    <pre className="whitespace-pre-wrap break-all">{resultText}</pre>
                  ) : (
                    <MarkdownText type="text" text={resultText} status={completeStatus} />
                  )}
                </div>
              )}
            </>
          )}
        </div>
      )}
    </div>
  );
};

// SubParts renders a subagent's ordered parts with the same components used for
// a top-level assistant message: reasoning → Reasoning, text → MarkdownText,
// tool → ToolCall (recursing for a nested spawn_agent).
const SubParts: FC<{ parts: SubPart[]; running: boolean }> = ({ parts, running }) => (
  <>
    {parts.map((p, i) => {
      if (p.kind === "thinking") {
        const partRunning = running && i === parts.length - 1;
        return (
          <Reasoning
            key={i}
            type="reasoning"
            text={p.text}
            status={partRunning ? { type: "running" } : { type: "complete" }}
          />
        );
      }
      if (p.kind === "text") {
        return <MarkdownText key={i} type="text" text={p.text} status={completeStatus} />;
      }
      // tool part: render via the same dispatcher (recursing for spawn_agent).
      return (
        <ToolPart key={p.id} part={p} />
      );
    })}
  </>
);

// NestedReplay renders a persisted sub-conversation (a tool-call part's
// `messages`, restored on reload) with the same components as a live one.
const NestedReplay: FC<{ messages: readonly ThreadMessage[] }> = ({ messages }) => (
  <>
    {messages.map((m, mi) =>
      m.content.map((part, pi) => {
        const key = `${mi}:${pi}`;
        if (part.type === "reasoning") {
          return <Reasoning key={key} type="reasoning" text={part.text} status={completeStatus} />;
        }
        if (part.type === "text") {
          return <MarkdownText key={key} type="text" text={part.text} status={completeStatus} />;
        }
        if (part.type === "tool-call") {
          return <Dispatch key={key} {...(part as ToolCallMessagePartProps)} />;
        }
        return null;
      }),
    )}
  </>
);

// completeStatus is the status given to finished nested parts (text/thinking)
// and tool calls, matching what a completed top-level part carries.
const completeStatus = { type: "complete" } as const;

// noop callbacks satisfy ToolCallMessagePartProps for our read-only nested
// rendering; nested tool results are driven by the backend, not the renderer.
const noop = () => {};

// ToolPart adapts a live subagent tool part to the props the dispatcher expects,
// reusing the exact top-level tool-call rendering.
const ToolPart: FC<{ part: Extract<SubPart, { kind: "tool" }> }> = ({ part }) => (
  <Dispatch
    type="tool-call"
    toolName={part.toolName}
    argsText={part.argsText}
    result={part.result}
    isError={part.isError}
    toolCallId={part.id}
    args={{}}
    status={part.status === "running" ? { type: "running" } : completeStatus}
    addResult={noop}
    resume={noop}
    respondToApproval={noop}
  />
);

const Dispatch = dispatch;

/* ---------- regular tool call ---------- */

const GenericCall: FC<ToolCallMessagePartProps> = (props) => {
  const { toolName, argsText, result, isError, status } = props;
  const running = status?.type === "running";
  const [open, setOpen] = useState(false);
  const expanded = running || open;

  useReport(props, running);

  const resultText = toText(result);

  return (
    <div
      className={`mb-2 rounded-xl border text-sm ${
        isError ? "border-red-200 bg-red-50" : "border-neutral-200 bg-neutral-50"
      }`}
    >
      <Header
        name={toolName}
        running={running}
        isError={isError}
        expanded={expanded}
        onToggle={() => setOpen((o) => !o)}
      />
      {expanded && (
        <div className="space-y-2 border-t border-neutral-200 px-3 py-2 font-mono text-xs leading-relaxed">
          {argsText && (
            <div>
              <div className="mb-1 font-sans text-neutral-400">arguments</div>
              <pre className="whitespace-pre-wrap break-all text-neutral-600">{argsText}</pre>
            </div>
          )}
          {(resultText || isError) && (
            <div>
              <div className="mb-1 font-sans text-neutral-400">result</div>
              <pre className={`whitespace-pre-wrap break-all ${isError ? "text-red-600" : "text-neutral-600"}`}>
                {resultText || "(no output)"}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
};

/* ---------- shared bits ---------- */

function toText(result: unknown): string {
  if (result === undefined || result === null) return "";
  return typeof result === "string" ? result : JSON.stringify(result, null, 2);
}

// useReport publishes the call to the right panel's Runs/Workspace feed,
// re-reporting as it streams running → done (upserts by toolCallId).
function useReport(
  { toolName, argsText, result, isError, toolCallId }: ToolCallMessagePartProps,
  running: boolean,
) {
  useEffect(() => {
    reportToolCall({
      id: toolCallId ?? `${toolName}`,
      toolName,
      argsText: argsText ?? "",
      result,
      isError,
      status: running ? "running" : isError ? "error" : "done",
    });
  }, [toolCallId, toolName, argsText, result, isError, running]);
}

const Header: FC<{
  name: string;
  running: boolean;
  isError?: boolean;
  expanded: boolean;
  onToggle: () => void;
  icon?: React.ReactNode;
  badge?: string;
}> = ({ name, running, isError, expanded, onToggle, icon, badge }) => (
  <button
    type="button"
    onClick={onToggle}
    className="flex w-full items-center gap-2 px-3 py-2 text-left text-neutral-500 hover:text-neutral-700"
  >
    <span
      className={`inline-block h-2 w-2 rounded-full ${
        running ? "animate-pulse bg-violet-500" : isError ? "bg-red-400" : "bg-emerald-400"
      }`}
    />
    {icon}
    <span className="font-mono font-medium">{name}</span>
    {badge && (
      <span className="rounded bg-violet-100 px-1 text-[10px] font-medium text-violet-600">{badge}</span>
    )}
    <span className="text-xs text-neutral-400">{running ? "running…" : isError ? "error" : "done"}</span>
    <span className="ml-auto text-xs text-neutral-400">{expanded ? "▾" : "▸"}</span>
  </button>
);
