import { useRef, useState, type FC } from "react";
import type { ReasoningMessagePartProps } from "@assistant-ui/react";
import { ChevronDown, ChevronRight } from "lucide-react";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";

/**
 * Renders the model's chain-of-thought as a collapsible block above the
 * answer. Default collapsed; auto-expands while the model is streaming its
 * thinking so the user can watch, then auto-collapses again when thinking
 * completes. The toggle overrides the auto state in both directions: the user
 * can collapse mid-think, or re-open a finished thought.
 */
export const Reasoning: FC<ReasoningMessagePartProps> = ({ text, status }) => {
  const running = status?.type === "running";
  // open is the user's manual override only (default collapsed = false). While
  // running, force-expanded UNLESS the user just toggled it shut; when thinking
  // finishes, running flips false and the block falls back to `open` (collapsed
  // unless the user pinned it open) — giving the auto-collapse for free.
  const [open, setOpen] = useState(false);
  // Tracks a manual close during THIS thinking phase so the running
  // force-expand doesn't immediately re-open it. Reset on each new running
  // phase so a later think auto-expands again.
  const [closedWhileRunning, setClosedWhileRunning] = useState(false);
  const wasRunning = useRef(false);
  if (running && !wasRunning.current) setClosedWhileRunning(false);
  wasRunning.current = running;

  const expanded = running ? open || !closedWhileRunning : open;

  if (!text) return null;

  const toggle = (next: boolean) => {
    setOpen(next);
    if (running) setClosedWhileRunning(!next);
  };

  const label = (() => {
    if (running && expanded) return t("chat.reasoningThinkingExpanded");
    if (running && !expanded) return t("chat.reasoningThinkingCollapsed");
    if (!running && expanded) return t("chat.reasoningDoneExpanded");
    return t("chat.reasoningDoneCollapsed");
  })();

  return (
    <Collapsible open={expanded} onOpenChange={toggle} className="mb-2 w-full max-w-full text-sm">
      <CollapsibleTrigger className="inline-flex items-center gap-1.5 py-1 text-left text-muted-foreground transition-colors hover:text-foreground">
        {expanded ? <ChevronDown className="size-3.5 shrink-0" /> : <ChevronRight className="size-3.5 shrink-0" />}
        <span className={cn("inline-block size-2 rounded-full", running ? "animate-pulse bg-primary" : "bg-muted-foreground/40")} />
        <span className="font-medium">{label}</span>
      </CollapsibleTrigger>
      <CollapsibleContent className="mt-1 w-full pl-5 font-mono text-xs leading-relaxed whitespace-pre-wrap text-muted-foreground">
        {text}
      </CollapsibleContent>
    </Collapsible>
  );
};
