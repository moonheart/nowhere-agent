import { useEffect, useRef, useState, type FC } from "react";
import type { ThreadMessage, ToolCallMessagePartProps } from "@assistant-ui/react";
import { Bot, HelpCircle, ShieldAlert } from "lucide-react";
import { reportToolCall, useActivity, type SubPart } from "@/lib/activity";
import {
  useApproval,
  respondToApproval,
  respondToAskUser,
  clearApproval,
  followDecisionStream,
  parseQuestions,
  type ToolApproval,
} from "@/lib/approval";
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
  const { toolName, argsText, result, isError, status, toolCallId } = props;
  const running = status?.type === "running";
  const [open, setOpen] = useState(false);
  const expanded = running || open;
  // A parked approval for this call (O2): the backend is holding the run until
  // the user approves/denies. Sourced from the transient data-tool-approval frame.
  const approval = useApproval(toolCallId);

  useReport(props, running);

  const resultText = toText(result);

  return (
    <div
      className={`mb-2 rounded-xl border text-sm ${
        isError ? "border-red-200 bg-red-50" : approval ? "border-amber-300 bg-amber-50/60" : "border-neutral-200 bg-neutral-50"
      }`}
    >
      <Header
        name={toolName}
        running={running}
        isError={isError}
        expanded={expanded}
        onToggle={() => setOpen((o) => !o)}
      />
      {approval && approval.kind === "ask_user" ? (
        <AskUserGate approval={approval} />
      ) : approval ? (
        <ApprovalGate approval={approval} argsText={argsText} />
      ) : null}
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

// ApprovalGate renders the approve/deny prompt for a parked tool call. Deciding
// POSTs the verdict to the backend (which resumes the run); the card clears the
// prompt and the resumed stream drives the rest of the render.
const ApprovalGate: FC<{ approval: { approvalId: string; toolCallId: string; toolName: string; args?: unknown }; argsText?: string }> = ({
  approval,
  argsText,
}) => {
  const [busy, setBusy] = useState<"approve" | "deny" | null>(null);
  const decide = async (approved: boolean) => {
    setBusy(approved ? "approve" : "deny");
    const stream = await respondToApproval(approval.approvalId, approved);
    if (stream) {
      clearApproval(approval.toolCallId);
      followDecisionStream(stream); // watch the resumed run continue live
    } else {
      setBusy(null); // backend rejected (already decided / not waiting) — keep the prompt
    }
  };
  return (
    <div className="border-t border-amber-200 px-3 py-2.5">
      <div className="flex items-start gap-2">
        <ShieldAlert size={15} className="mt-0.5 shrink-0 text-amber-500" />
        <div className="min-w-0 flex-1">
          <p className="text-[13px] font-medium text-amber-800">
            Approve running <span className="font-mono">{approval.toolName}</span>?
          </p>
          {argsText && (
            <pre className="mt-1 max-h-24 overflow-y-auto whitespace-pre-wrap break-all rounded bg-amber-100/60 p-1.5 font-mono text-[11px] text-amber-900">
              {argsText}
            </pre>
          )}
          <div className="mt-2 flex gap-2">
            <button
              type="button"
              disabled={busy !== null}
              onClick={() => void decide(true)}
              className="rounded-lg bg-amber-600 px-3 py-1 text-xs font-medium text-white transition-colors hover:bg-amber-700 disabled:opacity-50"
            >
              {busy === "approve" ? "Approving…" : "Approve"}
            </button>
            <button
              type="button"
              disabled={busy !== null}
              onClick={() => void decide(false)}
              className="rounded-lg border border-amber-300 bg-white px-3 py-1 text-xs font-medium text-amber-700 transition-colors hover:bg-amber-100 disabled:opacity-50"
            >
              {busy === "deny" ? "Denying…" : "Deny"}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
};

// AskUserGate renders the model's ask_user questions as one card: each question
// is a single- or multi-select over its options (recommended option
// pre-highlighted), with a per-question custom-answer box, a Submit that posts
// all answers, and a Skip that lets the model proceed without input (cancel =
// skip, the run continues). Capability O-ask.
const AskUserGate: FC<{ approval: ToolApproval }> = ({ approval }) => {
  const questions = parseQuestions(approval);
  // answers[q] = the chosen label(s) or custom text for that question.
  const [answers, setAnswers] = useState<Record<number, string[]>>({});
  const [custom, setCustom] = useState<Record<number, string>>({});
  const [busy, setBusy] = useState(false);

  const toggle = (qi: number, label: string, multi: boolean) => {
    setAnswers((prev) => {
      const cur = prev[qi] ?? [];
      const next = multi
        ? cur.includes(label)
          ? cur.filter((l) => l !== label)
          : [...cur, label]
        : [label];
      return { ...prev, [qi]: next };
    });
    setCustom((prev) => ({ ...prev, [qi]: "" })); // picking an option clears custom
  };

  const setCustomAnswer = (qi: number, text: string) => {
    setCustom((prev) => ({ ...prev, [qi]: text }));
    if (text) setAnswers((prev) => ({ ...prev, [qi]: [] })); // typing custom clears options
  };

  const submit = async () => {
    setBusy(true);
    const out: Record<string, string | string[]> = {};
    questions.forEach((q, qi) => {
      const c = (custom[qi] ?? "").trim();
      const sel = answers[qi] ?? [];
      if (c) out[q.question] = c;
      else if (sel.length === 1) out[q.question] = sel[0];
      else if (sel.length > 1) out[q.question] = sel;
    });
    const stream = await respondToAskUser(approval.approvalId, out);
    if (stream) {
      clearApproval(approval.toolCallId);
      followDecisionStream(stream);
    } else {
      setBusy(false);
    }
  };

  const skip = async () => {
    setBusy(true);
    const stream = await respondToAskUser(approval.approvalId, null);
    if (stream) {
      clearApproval(approval.toolCallId);
      followDecisionStream(stream);
    } else {
      setBusy(false);
    }
  };

  if (questions.length === 0) return null;
  return (
    <div className="border-t border-violet-200 bg-violet-50/50 px-3 py-2.5">
      <div className="flex items-start gap-2">
        <HelpCircle size={15} className="mt-0.5 shrink-0 text-violet-500" />
        <div className="min-w-0 flex-1 space-y-3">
          {questions.map((q, qi) => {
            const chosen = answers[qi] ?? [];
            const customText = custom[qi] ?? "";
            return (
              <div key={qi}>
                <p className="text-[13px] font-medium text-violet-900">
                  {q.header && (
                    <span className="mr-1.5 rounded bg-violet-100 px-1 text-[10px] font-medium text-violet-600">
                      {q.header}
                    </span>
                  )}
                  {q.question}
                </p>
                <div className="mt-1.5 flex flex-wrap gap-1.5">
                  {q.options.map((opt) => {
                    const active = chosen.includes(opt.label);
                    return (
                      <button
                        key={opt.label}
                        type="button"
                        disabled={busy}
                        title={opt.description}
                        onClick={() => toggle(qi, opt.label, !!q.multiselect)}
                        className={`rounded-lg border px-2.5 py-1 text-xs transition-colors ${
                          active
                            ? "border-violet-500 bg-violet-600 text-white"
                            : opt.recommended
                              ? "border-violet-300 bg-white text-violet-700 hover:bg-violet-100"
                              : "border-neutral-300 bg-white text-neutral-600 hover:bg-neutral-100"
                        }`}
                      >
                        {opt.label}
                        {opt.recommended && !active && <span className="ml-1 text-[9px] text-violet-400">★</span>}
                      </button>
                    );
                  })}
                </div>
                <input
                  type="text"
                  value={customText}
                  disabled={busy}
                  onChange={(e) => setCustomAnswer(qi, e.target.value)}
                  placeholder="Or type your own answer…"
                  className="mt-1.5 w-full rounded-lg border border-neutral-300 bg-white px-2.5 py-1 text-xs text-neutral-700 outline-none placeholder:text-neutral-400 focus:border-violet-400"
                />
              </div>
            );
          })}
          <div className="flex gap-2 pt-1">
            <button
              type="button"
              disabled={busy}
              onClick={() => void submit()}
              className="rounded-lg bg-violet-600 px-3 py-1 text-xs font-medium text-white transition-colors hover:bg-violet-700 disabled:opacity-50"
            >
              {busy ? "Sending…" : "Submit"}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => void skip()}
              className="rounded-lg border border-neutral-300 bg-white px-3 py-1 text-xs font-medium text-neutral-500 transition-colors hover:bg-neutral-100 disabled:opacity-50"
            >
              Skip
            </button>
          </div>
        </div>
      </div>
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
