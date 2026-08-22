import { useEffect, useLayoutEffect, useMemo, useRef, useState, type FC } from "react";
import { useMessage, useMessageTiming, useThread, useThreadRuntime, useThreadViewport } from "@assistant-ui/react";
import type { ThreadAssistantMessagePart, ThreadMessage } from "@assistant-ui/react";
import { CheckCircle2, ChevronDown, ChevronRight, LoaderCircle } from "lucide-react";
import { Reasoning } from "@/components/reasoning";
import { ToolCall } from "@/components/tool-call";
import { MarkdownText } from "@/components/markdown-text";
import { MessageImage } from "@/components/message-image";
import { DataUI, GenerativeUIFromMetadata } from "@/components/generative-ui";
import { UsageFooter } from "@/components/usage-footer";
import { Collapsible, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";
import { CopyButton } from "@/components/copy-button";

function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "0s";
  if (ms < 1000) return `${(ms / 1000).toFixed(1)}s`;
  const totalSec = Math.floor(ms / 1000);
  if (totalSec < 60) return `${(totalSec)}s`;
  const m = Math.floor(totalSec / 60);
  const s = totalSec % 60;
  return `${m}m ${s}s`;
}

function useElapsed(
  isRunning: boolean,
  timing: ReturnType<typeof useMessageTiming>,
  createdAt: Date | undefined,
): string | undefined {
  const fallbackStartRef = useRef<number | null>(null);
  const [now, setNow] = useState(() => Date.now());

  // capture fallback start on first running
  useEffect(() => {
    if (isRunning && fallbackStartRef.current == null) {
      const ca = createdAt ? createdAt.getTime() : Date.now();
      fallbackStartRef.current = ca;
    }
  }, [isRunning, createdAt]);

  useEffect(() => {
    if (!isRunning) return;
    const id = setInterval(() => setNow(Date.now()), 200);
    return () => clearInterval(id);
  }, [isRunning]);

  // 服务端持久化优先：已完成时 totalStreamTime 来自 runs.finished_at（history 回显）
  if (timing?.totalStreamTime != null && !isRunning) {
    return formatDuration(timing.totalStreamTime);
  }
  if (timing?.streamStartTime != null) {
    const start = timing.streamStartTime;
    const end = isRunning ? now : (timing.totalStreamTime != null ? start + timing.totalStreamTime : now);
    const ms = end - start;
    if (ms >= 0) return formatDuration(ms);
  }
  if (fallbackStartRef.current != null) {
    const ms = now - fallbackStartRef.current;
    return formatDuration(ms);
  }
  if (createdAt && isRunning) {
    const ms = now - createdAt.getTime();
    return formatDuration(ms);
  }
  return undefined;
}



function LeadPartRenderer({ part }: { part: ThreadAssistantMessagePart }) {
  switch (part.type) {
    case "text":
      return <MarkdownText type="text" text={(part as any).text} status={(part as any).status} />;
    case "reasoning":
      return <Reasoning type="reasoning" text={(part as any).text} status={(part as any).status} />;
    case "tool-call":
      return <ToolCall {...(part as any)} />;
    case "image":
      return <MessageImage {...(part as any)} />;
    case "data":
      return <DataUI name={(part as any).name} data={(part as any).data} />;
    case "generative-ui":
      // generative-ui parts are handled via DataUI / SpecTree; fallback
      return <DataUI name="generative-ui" data={(part as any).spec ? { spec: (part as any).spec } : (part as any).data} />;
    case "source":
      return (
        <div className="text-xs text-muted-foreground">
          {(part as any).url ? (
            <a href={(part as any).url} target="_blank" rel="noreferrer" className="underline">
              {(part as any).title ?? (part as any).url}
            </a>
          ) : (
            (part as any).title ?? "来源"
          )}
        </div>
      );
    case "file":
      return (
        <div className="text-xs text-muted-foreground">
          {(part as any).filename ?? "文件"} · {(part as any).mimeType ?? ""}
        </div>
      );
    default:
      return <div className="text-xs text-muted-foreground">未知类型 {(part as any).type}</div>;
  }
}

/**
 * AssistantTurn — per-round container.
 * Header: 可折叠的“处理中/已处理”+ 计时，内容按 step 展示。
 * 完成时 1..N-1 折叠，最后一条（必为正文）外显。
 *
 * 当 N==1 且为纯文本且已完成时，隐藏 header，直接外显正文以保持简洁。
 */
export const AssistantTurn: FC = () => {
  const content = useMessage((s: any) => (s.content ?? []) as readonly ThreadAssistantMessagePart[]);
  const status = useMessage((s: any) => s.status as { type: string; reason?: string; error?: unknown } | undefined);
  const metadata = useMessage((s: any) => s.metadata as any);
  const createdAt = useMessage((s: any) => (s.createdAt ?? s.created_at) as Date | undefined);
  const timing = useMessageTiming();

  const isRunning = status?.type === "running" || status?.type === "requires-action";
  const isComplete = status?.type === "complete";
  const isIncomplete = status?.type === "incomplete";
  const messageId = useMessage((s: any) => (s.id ?? "") as string);

  const elapsed = useElapsed(!!isRunning, timing as any, createdAt);

  // 保持视口贴底：流式时若用户已在底部，随内容增长自动 scrollToBottom，修复“向下撑开”
  // 用 useLayoutEffect + instant 使扩容在绘制前完成，避免先撑开再跳动的闪动
  const { isAtBottom, scrollToBottom } = useThreadViewport();
  useLayoutEffect(() => {
    if (isRunning && isAtBottom) {
      scrollToBottom({ behavior: "instant" } as any);
    }
  }, [content, isRunning, isAtBottom, scrollToBottom]);

  const total = content.length;

  const { leadSteps, finalPart } = useMemo(() => {
    if (total === 0) return { leadSteps: [] as typeof content, finalPart: null as ThreadAssistantMessagePart | null };
    const last = content[total - 1] as ThreadAssistantMessagePart;
    const lastIsText = last.type === "text";
    // 按需求：最后一条一定是正文才外显，否则全部视为 lead
    if (lastIsText && total > 1) {
      return { leadSteps: content.slice(0, -1), finalPart: last };
    }
    if (lastIsText && total === 1) {
      // 单条文本：若正在运行也视为 final（外显流式文本），不展示 lead
      return { leadSteps: [] as typeof content, finalPart: last };
    }
    // 最后一条不是文本（仍在工具/思考阶段），全部放入 lead，外显为空
    return { leadSteps: content, finalPart: null as ThreadAssistantMessagePart | null };
  }, [content, total]);

  const hasLead = leadSteps.length > 0;
  const showHeader = hasLead || !!isRunning;

  // Collapsible open state: 运行时默认展开，完成后默认折叠，用户可手动切换。
  // isOpen = 用户显式切换 ?? 是否运行中，天然实现“完成后自动收起”
  const [manualOpen, setManualOpen] = useState<boolean | undefined>(undefined);
  const isOpen = manualOpen ?? !!isRunning;

  // Reset manual when message id changes (new turn). User's explicit expand/collapse should not leak to next turn.
  const lastIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (messageId && lastIdRef.current !== messageId) {
      lastIdRef.current = messageId;
      setManualOpen(undefined);
    }
  }, [messageId]);

  // 单步纯文本完成态：无需 Turn 容器，直接渲染正文（保持原有气泡样式）
  if (!showHeader && finalPart) {
    return (
      <div className="w-full group/copy">
        <div className="markdown-body leading-relaxed">
          <MarkdownText type="text" text={(finalPart as any).text} status={(finalPart as any).status} />
        </div>
        <GenerativeUIFromMetadata />
        <div className="mt-1 flex items-center justify-between gap-2 border-t border-border pt-1.5">
          <UsageFooter className="mt-0 flex-1 border-0 pt-0" />
          <CopyButton text={(finalPart as any).text as string} withLabel className="shrink-0 opacity-0 transition-opacity group-hover/copy:opacity-100 group-focus-within/copy:opacity-100" />
        </div>
        <InlineFailedNotice metadata={metadata} status={status} />
      </div>
    );
  }

  // 无内容但运行中：展示处理中 header + 空步骤占位
  return (
    <div className="w-full">
      {showHeader && (
        <Collapsible open={hasLead ? isOpen : false} onOpenChange={(o) => setManualOpen(o)} className="group w-full">
          <CollapsibleTrigger className="flex w-full items-center justify-start gap-2 bg-transparent px-0 py-0.5 text-left text-sm transition-colors hover:bg-transparent border-0 shadow-none focus-visible:ring-0 focus-visible:outline-none group">
            {isRunning ? (
              <LoaderCircle className="size-4 shrink-0 animate-spin text-primary" />
            ) : isIncomplete ? (
              <LoaderCircle className="size-4 shrink-0 text-amber-600" />
            ) : (
              <CheckCircle2 className="size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
            )}
            <span className={cn("text-sm font-medium", isRunning ? "text-primary" : "text-foreground")}>
              {isRunning ? t("chat.processing") : isIncomplete ? t("chat.notCompleted") : t("chat.processed")}
            </span>
            {elapsed && (
              <span className="text-xs text-muted-foreground">
                · {isRunning ? `${elapsed}` : `耗时 ${elapsed}`}
              </span>
            )}
            {hasLead && (
              <span className="ml-1 rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">
                {t("chat.stepsCount", { count: String(leadSteps.length) })}
              </span>
            )}
            {hasLead && (
              <span className="ml-1 flex items-center text-muted-foreground opacity-0 transition-opacity duration-150 group-hover:opacity-100 group-focus-visible:opacity-100">
                {isOpen ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
              </span>
            )}
          </CollapsibleTrigger>

          {hasLead && isOpen && (
            <div className="mt-1 space-y-2">
              {leadSteps.map((part, i) => (
                <LeadPartRenderer key={`${(part as any).toolCallId ?? (part as any).type}-${i}`} part={part as ThreadAssistantMessagePart} />
              ))}
              {!finalPart && (
                <div className="pb-2">
                  <GenerativeUIFromMetadata />
                </div>
              )}
            </div>
          )}
        </Collapsible>
      )}

      {/* 最后一条正文外显：正文 + 消耗线 + 复制按钮（与消耗同行、靠右、hover 显示） */}
      {finalPart && (
        <div className={cn("mt-1 group/copy", showHeader && "border-t border-border/60 pt-1")}>
          <MarkdownText type="text" text={(finalPart as any).text} status={(finalPart as any).status} />
          <GenerativeUIFromMetadata />
          <div className="mt-1 flex items-center justify-between gap-2 border-t border-border pt-1.5">
            <UsageFooter className="mt-0 flex-1 border-0 pt-0" />
            <CopyButton text={(finalPart as any).text as string} withLabel className="shrink-0 opacity-0 transition-opacity group-hover/copy:opacity-100 group-focus-within/copy:opacity-100" />
          </div>
          <InlineFailedNotice metadata={metadata} status={status} />
        </div>
      )}

      {/* 若没有 finalPart（仍在工具阶段），把 Usage/Failed 放在steps下方 */}
      {!finalPart && (isComplete || isIncomplete) && (
        <div className="mt-2">
          <UsageFooter />
          <InlineFailedNotice metadata={metadata} status={status} />
        </div>
      )}
      {/* 运行中且无 finalPart：保持 steps 展开，无需额外外显 */}
      {!finalPart && isRunning && hasLead && (
        <div className="mt-2">
          <UsageFooter />
        </div>
      )}
    </div>
  );
};

// Inline failed notice — keeps the same UX for history vs live failed runs,
// plus a Retry that re-submits the preceding user message.
function InlineFailedNotice({ metadata, status }: { metadata: any; status: any }) {
  const metaError = (() => {
    const e = metadata?.error;
    return typeof e === "string" && e.length > 0 ? e : null;
  })();
  const liveError = (() => {
    const st = status;
    if (!st || st.type !== "incomplete" || st.reason === "cancelled") return null;
    const e = st.error;
    if (typeof e === "string" && e.length > 0) return e;
    if (e && typeof e === "object" && "message" in e && typeof (e as any).message === "string" && (e as any).message.length > 0) return (e as any).message;
    return st.reason === "error" || st.reason === "length" ? "The reply ended before completion." : null;
  })();
  const errorText = liveError ?? metaError;
  const msgId = useMessage((s: any) => (s.id ?? "") as string);
  const threadRuntime = useThreadRuntime();
  const isRunning = useThread((s: any) => s.isRunning as boolean);
  if (!errorText) return null;

  const retry = () => {
    if (isRunning) return;
    const msgs = threadRuntime.getState().messages as readonly ThreadMessage[];
    const idx = msgs.findIndex((m) => m.id === msgId);
    for (let i = idx - 1; i >= 0; i--) {
      const m = msgs[i];
      if (m.role !== "user") continue;
      const text = (m.content as readonly { type: string; text?: string }[])
        .filter((p): p is { type: "text"; text: string } => p.type === "text")
        .map((p) => p.text)
        .join("\n");
      if (!text) continue;
      const draft = threadRuntime.composer.getState().text.trim();
      threadRuntime.composer.setText(draft ? `${draft}\n${text}` : text);
      threadRuntime.composer.send();
      return;
    }
  };

  return (
    <div className="mt-3 flex items-center gap-3 rounded-lg border border-red-600/30 bg-red-500/10 px-3 py-2">
      <p className="min-w-0 flex-1 text-xs text-muted-foreground">{errorText}</p>
      <Button type="button" variant="outline" size="sm" disabled={isRunning} title={t("chat.rerunTitle")} onClick={retry} className="shrink-0">
        {t("chat.retry")}
      </Button>
    </div>
  );
}


