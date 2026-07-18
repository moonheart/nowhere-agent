import { useState, type FC } from "react";
import type { ReasoningMessagePartProps } from "@assistant-ui/react";

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
    <div className="mb-2 rounded-xl border border-neutral-200 bg-neutral-50 text-sm">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left text-neutral-500 hover:text-neutral-700"
      >
        <span
          className={`inline-block h-2 w-2 rounded-full ${
            running ? "animate-pulse bg-violet-500" : "bg-neutral-300"
          }`}
        />
        <span className="font-medium">
          {running ? "Thinking…" : "Thought process"}
        </span>
        <span className="ml-auto text-xs text-neutral-400">
          {expanded ? "▾" : "▸"}
        </span>
      </button>
      {expanded && (
        <div className="whitespace-pre-wrap border-t border-neutral-200 px-3 py-2 font-mono text-xs leading-relaxed text-neutral-600">
          {text}
        </div>
      )}
    </div>
  );
};
