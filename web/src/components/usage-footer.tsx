import type { FC } from "react";
import { useMessage } from "@assistant-ui/react";
import { cn } from "@/lib/utils";

type UsageData = {
  inputTokens?: number;
  outputTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
};

// pickUsage pulls the token-usage entry out of the message's unstable_data
// (populated live by the server's data-usage frame, and on history reload by
// the /api/chat/history metadata). Returns null when the message has none
// (user turns, or a run that reported no usage).
function pickUsage(metadata: unknown): UsageData | null {
  const list = (metadata as { unstable_data?: unknown[] } | undefined)
    ?.unstable_data;
  if (!Array.isArray(list)) return null;
  for (const entry of list) {
    const e = entry as { name?: string; data?: UsageData } | undefined;
    if (e?.name === "usage" && e.data && typeof e.data.inputTokens === "number") {
      return e.data;
    }
  }
  return null;
}

function fmt(n: number): string {
  return n.toLocaleString("en-US");
}

/**
 * Renders a compact token-usage footer under an assistant reply: input/output
 * tokens plus the prompt-prefix cache hit rate when caching was active.
 */
export const UsageFooter: FC<{ className?: string }> = ({ className }) => {
  const usage = useMessage((s) => pickUsage(s.metadata));
  if (!usage) return null;

  const input = usage.inputTokens ?? 0;
  const output = usage.outputTokens ?? 0;
  const cacheRead = usage.cacheReadTokens ?? 0;
  // Hit rate = cached reads over the TOTAL prompt tokens. On DeepSeek/OpenAI
  // inputTokens already includes the cached prefix (prompt_tokens ⊇ cached),
  // so the share is cacheRead / input — NOT cacheRead / (cacheRead + input),
  // which would double-count the cached portion. Kept to 2 decimal places.
  const hitPct =
    cacheRead > 0 && input > 0
      ? Math.min(100, (cacheRead * 100) / input).toFixed(2)
      : null;

  return (
    <div className={cn("mt-2 flex items-center gap-1.5 border-t border-border pt-1.5 text-[11px] text-muted-foreground", className)}>
      <span title="input tokens">↑{fmt(input)}</span>
      <span aria-hidden>·</span>
      <span title="output tokens">↓{fmt(output)}</span>
      {hitPct !== null && (
        <>
          <span aria-hidden>·</span>
          <span title={`prompt cache: ${fmt(cacheRead)} tokens reused`}>
            ⚡{hitPct}% cached
          </span>
        </>
      )}
    </div>
  );
};
