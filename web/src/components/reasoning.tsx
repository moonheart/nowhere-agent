import { useState, type FC } from "react";
import type { ReasoningMessagePartProps } from "@assistant-ui/react";
import { ChevronDown, ChevronRight } from "lucide-react";
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible";
import { cn } from "@/lib/utils";

/**
 * Renders the model's chain-of-thought as a collapsible block above the
 * answer. While the run is streaming it stays open so the user can watch the
 * model think; once complete it collapses to a one-line summary toggle.
 */
export const Reasoning: FC<ReasoningMessagePartProps> = ({ text, status }) => {
  const running = status?.type === "running";
  const [open, setOpen] = useState(true);
  const expanded = running || open;

  if (!text) return null;

  return (
    <Collapsible
      open={expanded}
      onOpenChange={setOpen}
      className="mb-2 rounded-xl border border-border bg-muted/50 text-sm"
    >
      <CollapsibleTrigger className="flex w-full items-center gap-2 px-3 py-2 text-left text-muted-foreground transition-colors hover:text-foreground">
        <span
          className={cn(
            "inline-block size-2 rounded-full",
            running ? "animate-pulse bg-primary" : "bg-muted-foreground/40",
          )}
        />
        <span className="font-medium">
          {running ? "Thinking…" : "Thought process"}
        </span>
        {expanded ? (
          <ChevronDown className="ml-auto size-3.5" />
        ) : (
          <ChevronRight className="ml-auto size-3.5" />
        )}
      </CollapsibleTrigger>
      <CollapsibleContent className="border-t border-border px-3 py-2 font-mono text-xs leading-relaxed whitespace-pre-wrap text-muted-foreground">
        {text}
      </CollapsibleContent>
    </Collapsible>
  );
};
