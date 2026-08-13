import { useEffect, useRef, useState, type FC } from "react";
import type { ThreadMessage, ToolCallMessagePartProps } from "@assistant-ui/react";
import {
  Bot,
  ChevronDown,
  ChevronRight,
  HelpCircle,
  Laptop,
  LoaderCircle,
  ShieldAlert,
  TriangleAlert,
} from "lucide-react";
import { reportToolCall, useActivity, activityEpoch, type SubPart } from "@/lib/activity";
import { usePermissionMode } from "@/lib/permission";
import {
  useApproval,
  useApprovalFailure,
  usePendingInteractions,
  respondToApproval,
  respondToAskUser,
  clearApproval,
  hasPendingInteractions,
  followDecisionStream,
  parseQuestions,
  retryClientTool,
  type ToolApproval,
} from "@/lib/approval";
import { Reasoning } from "@/components/reasoning";
import { MarkdownText } from "@/components/markdown-text";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { Input } from "@/components/ui/input";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";

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
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className={cn(
        "mb-2 rounded-xl border text-sm",
        isError
          ? "border-destructive/30 bg-destructive/5"
          : "border-primary/30 bg-primary/5",
      )}
    >
      <CallHeader
        icon={<Bot className="size-3.5 shrink-0 text-primary" />}
        name={toolName}
        running={running}
        isError={isError}
        expanded={open}
        badge={live && live.depth > 1 ? `L${live.depth}` : undefined}
      />
      <CollapsibleContent className="max-h-96 space-y-2 overflow-y-auto border-t border-primary/20 px-3 py-2">
        {mode === "live" && (
          <>
            <SubParts parts={liveParts} running={running} />
            {running && <span className="animate-pulse text-primary/60">▍</span>}
          </>
        )}
        {mode === "replay" && <NestedReplay messages={replayMessages} />}
        {mode === "result" && (
          <>
            {running && (
              <div className="text-xs text-muted-foreground">subagent working…</div>
            )}
            {resultText && (
              <div className={isError ? "font-mono text-xs text-destructive" : ""}>
                {isError ? (
                  <pre className="break-all whitespace-pre-wrap">{resultText}</pre>
                ) : (
                  <MarkdownText type="text" text={resultText} status={completeStatus} />
                )}
              </div>
            )}
            {/* Calls predating nested persistence have no result AND no replay;
                without this the panel opens onto nothing and reads as broken. */}
            {!running && !resultText && (
              <div className="text-xs text-muted-foreground">(no output)</div>
            )}
          </>
        )}
      </CollapsibleContent>
    </Collapsible>
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
  // undefined = follow the default (open while running, closed when done);
  // true/false = the user's explicit toggle, which wins until the run ends.
  // This lets a user collapse a large tool call (e.g. a streaming write_file)
  // mid-run without the auto-expand forcing it back open on the next delta.
  const [manual, setManual] = useState<boolean | undefined>(undefined);
  const wasRunning = useRef(running);
  useEffect(() => {
    // Reset the manual override when the run ends, so the next streaming call
    // auto-expands again instead of staying stuck at the last toggle.
    if (wasRunning.current && !running) setManual(undefined);
    wasRunning.current = running;
  }, [running]);
  const expanded = manual ?? running;
  // A parked interaction for this call (general interrupt): the backend suspended
  // the run until the client responds — a dangerous-action approval, an ask_user
  // question set, or a client_tool the browser auto-runs. From the transient
  // data-interaction frame (or the durable pendingApproval echo on reload).
  const approval = useApproval(toolCallId);
  // A gated batch parks several cards at once; only the queue head is
  // actionable (decide it → it clears → the next promotes). Non-head cards show
  // a waiting note instead of live buttons. Mirrors the sequential-permission
  // UX (claude-code / pi): one prompt at a time, in order.
  const queue = usePendingInteractions();
  const isHead = approval !== undefined && queue.length > 0 && queue[0].toolCallId === approval.toolCallId;
  // When the session is in allow_all, the backend's permission middleware runs
  // gated calls without prompting, so no approval card should appear. Hide any
  // stale approval-kind card (e.g. a frame that arrived just before the toggle)
  // rather than leaving a dead prompt on a call that already executed. ask_user
  // and client_tool are not permission approvals — they still render.
  const permissionMode = usePermissionMode();
  const hideApproval = permissionMode === "allow_all" && approval?.kind !== "ask_user" && approval?.kind !== "client_tool";

  useReport(props, running);

  const resultText = toText(result);

  return (
    <Collapsible
      open={expanded}
      onOpenChange={setManual}
      className={cn(
        "mb-2 rounded-xl border text-sm",
        isError
          ? "border-destructive/30 bg-destructive/5"
          : approval?.kind === "client_tool"
            ? "border-sky-500/30 bg-sky-500/5"
            : approval
              ? "border-amber-500/40 bg-amber-500/5"
              : "border-border bg-muted/50",
      )}
    >
      <CallHeader
        name={toolName}
        running={running}
        isError={isError}
        expanded={expanded}
      />
      {approval?.kind === "ask_user" ? (
        isHead ? <AskUserGate approval={approval} /> : <QueuedNote />
      ) : approval?.kind === "client_tool" ? (
        <ClientToolGate approval={approval} />
      ) : approval && !hideApproval ? (
        isHead ? (
          <ApprovalGate approval={approval} argsText={argsText} />
        ) : (
          <QueuedNote />
        )
      ) : null}
      <CollapsibleContent className="space-y-2 border-t border-border px-3 py-2 font-mono text-xs leading-relaxed">
        {argsText && (
          <div>
            <div className="mb-1 font-sans text-muted-foreground">arguments</div>
            <pre className="break-all whitespace-pre-wrap text-foreground/70">
              {argsText}
            </pre>
          </div>
        )}
        {(resultText || isError) && (
          <div>
            <div className="mb-1 font-sans text-muted-foreground">result</div>
            <pre
              className={cn(
                "break-all whitespace-pre-wrap",
                isError ? "text-destructive" : "text-foreground/70",
              )}
            >
              {resultText || "(no output)"}
            </pre>
          </div>
        )}
      </CollapsibleContent>
    </Collapsible>
  );
};

// QueuedNote marks a gated call that is parked behind an earlier approval in a
// multi-call batch: it is not yet actionable, and becomes live once the head of
// the queue is decided. Rendered instead of the interactive gate.
const QueuedNote: FC = () => (
  <div className="flex items-center gap-2 border-t border-amber-500/30 px-3 py-2.5">
    <ShieldAlert className="size-4 shrink-0 text-amber-600/60 dark:text-amber-500/60" />
    <p className="text-[13px] text-muted-foreground">
      {t("approval.waitingEarlier")}
    </p>
  </div>
);

// ApprovalGate renders the approve/deny prompt for a parked tool call. Deciding
// POSTs the verdict to the backend (which resumes the run); the card clears the
// prompt and the resumed stream drives the rest of the render.
const ApprovalGate: FC<{ approval: ToolApproval; argsText?: string }> = ({
  approval,
  argsText,
}) => {
  const [busy, setBusy] = useState<"approve" | "deny" | null>(null);
  const [error, setError] = useState<string | null>(null);
  const decide = async (approved: boolean) => {
    setBusy(approved ? "approve" : "deny");
    setError(null);
    try {
      const stream = await respondToApproval(approval.interactionId, approved);
      if (stream) {
        clearApproval(approval.toolCallId);
        // Only watch the resumed run live when the batch is now complete (the
        // backend started a fresh run). While siblings are still queued the
        // backend did NOT resume — following its trivial no-content stream would
        // open an empty assistant bubble.
        if (!hasPendingInteractions()) followDecisionStream(stream);
      } else {
        setBusy(null); // resumed with no stream to follow — keep the prompt
      }
    } catch (err) {
      // A rejected verdict must not vanish silently: show why and re-enable.
      setError((err as Error).message || "the decision failed");
      setBusy(null);
    }
  };
  return (
    <div className="border-t border-amber-500/30 px-3 py-2.5">
      <div className="flex items-start gap-2">
        <ShieldAlert className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-500" />
        <div className="min-w-0 flex-1">
          <p className="text-[13px] font-medium text-amber-700 dark:text-amber-400">
            {t("approval.approveRunning", { tool: approval.toolName })}
          </p>
          {argsText && (
            <pre className="mt-1 max-h-24 overflow-y-auto rounded bg-amber-500/10 p-1.5 font-mono text-[11px] break-all whitespace-pre-wrap text-amber-800 dark:text-amber-300">
              {argsText}
            </pre>
          )}
          <div className="mt-2 flex gap-2">
            <Button
              size="sm"
              disabled={busy !== null}
              onClick={() => void decide(true)}
            >
              {busy === "approve" ? t("approval.approving") : t("approval.approve")}
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={busy !== null}
              onClick={() => void decide(false)}
            >
              {busy === "deny" ? t("approval.denying") : t("approval.deny")}
            </Button>
          </div>
          {error && (
            <p className="mt-2 rounded border border-destructive/30 bg-destructive/5 px-2 py-1 text-[12px] text-destructive">
              {error}
            </p>
          )}
        </div>
      </div>
    </div>
  );
};

// ClientToolGate renders the auto-executing state of a client-side tool call:
// the browser runs the declared capability (clipboard, timezone, …) and POSTs
// the output — approval.ts drives that automatically the moment the interaction
// frame arrives, so this card is purely a live indicator. When the output
// posts, the prompt clears and the resumed run streams the tool result into
// this same card. If the auto-run or verdict POST failed, the card shows why
// and offers a Retry that re-executes the capability (bypassing the once-only
// autoRan guard). If the browser capability is unavailable the run folds an
// is_error result instead and the model reacts to it.
const ClientToolGate: FC<{ approval: ToolApproval }> = ({ approval }) => {
  const failure = useApprovalFailure(approval.toolCallId);
  const [retrying, setRetrying] = useState(false);
  const retry = async () => {
    setRetrying(true);
    try {
      await retryClientTool(approval);
    } finally {
      setRetrying(false);
    }
  };
  return (
    <div
      className={cn(
        "flex items-start gap-2 border-t border-sky-500/30 px-3 py-2.5",
        failure && "border-destructive/30 bg-destructive/5",
      )}
    >
      {failure ? (
        <TriangleAlert className="mt-0.5 size-4 shrink-0 text-destructive" />
      ) : (
        <Laptop className="mt-0.5 size-4 shrink-0 animate-pulse text-sky-600 dark:text-sky-400" />
      )}
      {failure ? (
        <div className="min-w-0 flex-1">
          <p className="text-[13px] font-medium text-destructive">
            {t("clientTool.failedInBrowser", { tool: approval.toolName })}
          </p>
          <p className="mt-0.5 text-[12px] text-muted-foreground">{failure}</p>
          <Button
            size="sm"
            variant="outline"
            className="mt-2"
            disabled={retrying}
            onClick={() => void retry()}
          >
            {retrying ? t("clientTool.retrying") : t("clientTool.retry")}
          </Button>
        </div>
      ) : (
        <p className="text-[13px] text-sky-700 dark:text-sky-300">
          {t("clientTool.runningInBrowser", { tool: approval.toolName })}
        </p>
      )}
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
  const [error, setError] = useState<string | null>(null);

  const choose = (qi: number, next: string[]) => {
    setAnswers((prev) => ({ ...prev, [qi]: next }));
    setCustom((prev) => ({ ...prev, [qi]: "" })); // picking an option clears custom
  };

  const setCustomAnswer = (qi: number, text: string) => {
    setCustom((prev) => ({ ...prev, [qi]: text }));
    if (text) setAnswers((prev) => ({ ...prev, [qi]: [] })); // typing custom clears options
  };

  const submit = async () => {
    setBusy(true);
    setError(null);
    const out: Record<string, string | string[]> = {};
    questions.forEach((q, qi) => {
      const c = (custom[qi] ?? "").trim();
      const sel = answers[qi] ?? [];
      if (c) out[q.question] = c;
      else if (sel.length === 1) out[q.question] = sel[0];
      else if (sel.length > 1) out[q.question] = sel;
    });
    try {
      const stream = await respondToAskUser(approval.interactionId, out);
      if (stream) {
        clearApproval(approval.toolCallId);
        if (!hasPendingInteractions()) followDecisionStream(stream);
      } else {
        setBusy(false);
      }
    } catch (err) {
      setError((err as Error).message || "the answer could not be sent");
      setBusy(false);
    }
  };

  const skip = async () => {
    setBusy(true);
    setError(null);
    try {
      const stream = await respondToAskUser(approval.interactionId, null);
      if (stream) {
        clearApproval(approval.toolCallId);
        if (!hasPendingInteractions()) followDecisionStream(stream);
      } else {
        setBusy(false);
      }
    } catch (err) {
      setError((err as Error).message || "the skip could not be sent");
      setBusy(false);
    }
  };

  if (questions.length === 0) return null;
  return (
    <div className="border-t border-primary/20 bg-primary/5 px-3 py-2.5">
      <div className="flex items-start gap-2">
        <HelpCircle className="mt-0.5 size-4 shrink-0 text-primary" />
        <div className="min-w-0 flex-1 space-y-3">
          {questions.map((q, qi) => {
            const chosen = answers[qi] ?? [];
            const customText = custom[qi] ?? "";
            return (
              <div key={qi}>
                <p className="text-[13px] font-medium text-foreground">
                  {q.header && (
                    <Badge
                      variant="secondary"
                      className="mr-1.5 h-4 px-1 text-[10px]"
                    >
                      {q.header}
                    </Badge>
                  )}
                  {q.question}
                </p>
                <ToggleGroup
                  variant="outline"
                  size="sm"
                  multiple={!!q.multiselect}
                  disabled={busy}
                  value={chosen}
                  onValueChange={(next) => choose(qi, next)}
                  className="mt-1.5 flex-wrap"
                >
                  {q.options.map((opt) => (
                    <ToggleGroupItem
                      key={opt.label}
                      value={opt.label}
                      title={opt.description}
                      className={cn(
                        "bg-background aria-pressed:bg-primary aria-pressed:text-primary-foreground",
                        opt.recommended &&
                          "border-primary/40 text-primary hover:text-primary",
                      )}
                    >
                      {opt.label}
                      {opt.recommended && !chosen.includes(opt.label) && (
                        <span className="text-[9px] text-primary/70">★</span>
                      )}
                    </ToggleGroupItem>
                  ))}
                </ToggleGroup>
                <Input
                  type="text"
                  value={customText}
                  disabled={busy}
                  onChange={(e) => setCustomAnswer(qi, e.target.value)}
                  placeholder={t("ask.customPlaceholder")}
                  aria-label={t("ask.customAria")}
                  className="mt-1.5 h-7 bg-background text-xs"
                />
              </div>
            );
          })}
          <div className="flex gap-2 pt-1">
            <Button size="sm" disabled={busy} onClick={() => void submit()}>
              {busy ? t("ask.sending") : t("ask.submit")}
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => void skip()}
            >
              {t("ask.skip")}
            </Button>
          </div>
          {error && (
            <p className="rounded border border-destructive/30 bg-destructive/5 px-2 py-1 text-[12px] text-destructive">
              {error}
            </p>
          )}
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
//
// The epoch is captured ONCE (useState initializer) when this card mounts. A
// gated call resumes on a fresh backend run, and that re-stream can still be
// delivering this very part when the user switches / starts a new chat —
// resetActivity bumps the epoch and the store then drops these late reports,
// so the new conversation's panel isn't repopulated with the old run's rows.
function useReport(
  { toolName, argsText, result, isError, toolCallId }: ToolCallMessagePartProps,
  running: boolean,
) {
  const [epoch] = useState(() => activityEpoch());
  useEffect(() => {
    reportToolCall(
      {
        id: toolCallId ?? `${toolName}`,
        toolName,
        argsText: argsText ?? "",
        result,
        isError,
        status: running ? "running" : isError ? "error" : "done",
      },
      epoch,
    );
  }, [toolCallId, toolName, argsText, result, isError, running, epoch]);
}

// CallHeader is the always-visible row that toggles the block. It must sit
// inside a Collapsible: it IS the trigger.
const CallHeader: FC<{
  name: string;
  running: boolean;
  isError?: boolean;
  expanded: boolean;
  icon?: React.ReactNode;
  badge?: string;
}> = ({ name, running, isError, expanded, icon, badge }) => (
  <CollapsibleTrigger className="flex w-full items-center gap-2 px-3 py-2 text-left text-muted-foreground transition-colors hover:text-foreground">
    {running ? (
      <LoaderCircle className="size-3.5 shrink-0 animate-spin text-primary" />
    ) : (
      <span
        className={cn(
          "inline-block size-2 shrink-0 rounded-full",
          isError ? "bg-destructive" : "bg-emerald-500",
        )}
      />
    )}
    {icon}
    <span className="font-mono font-medium">{name}</span>
    {badge && (
      <Badge variant="secondary" className="h-4 px-1 text-[10px]">
        {badge}
      </Badge>
    )}
    <span className="text-xs">
      {running ? "running…" : isError ? "error" : "done"}
    </span>
    {expanded ? (
      <ChevronDown className="ml-auto size-3.5" />
    ) : (
      <ChevronRight className="ml-auto size-3.5" />
    )}
  </CollapsibleTrigger>
);
