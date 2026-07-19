import { useState, type FC } from "react";
import type { ToolCallMessagePartProps } from "@assistant-ui/react";

/**
 * Renders a tool call (file read/write, etc.) as a collapsible block in the
 * assistant message. The header always shows the tool name and live status;
 * expanding it reveals the arguments the model sent and the result the tool
 * returned. Errors are highlighted.
 */
export const ToolCall: FC<ToolCallMessagePartProps> = ({
  toolName,
  argsText,
  result,
  isError,
  status,
}) => {
  const running = status?.type === "running";
  const [open, setOpen] = useState(false);
  const expanded = running || open;

  const resultText =
    result === undefined || result === null
      ? ""
      : typeof result === "string"
        ? result
        : JSON.stringify(result, null, 2);

  return (
    <div
      className={`mb-2 rounded-xl border text-sm ${
        isError
          ? "border-red-200 bg-red-50"
          : "border-neutral-200 bg-neutral-50"
      }`}
    >
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left text-neutral-500 hover:text-neutral-700"
      >
        <span
          className={`inline-block h-2 w-2 rounded-full ${
            running
              ? "animate-pulse bg-violet-500"
              : isError
                ? "bg-red-400"
                : "bg-emerald-400"
          }`}
        />
        <span className="font-mono font-medium">{toolName}</span>
        <span className="text-xs text-neutral-400">
          {running ? "running…" : isError ? "error" : "done"}
        </span>
        <span className="ml-auto text-xs text-neutral-400">
          {expanded ? "▾" : "▸"}
        </span>
      </button>
      {expanded && (
        <div className="space-y-2 border-t border-neutral-200 px-3 py-2 font-mono text-xs leading-relaxed">
          {argsText && (
            <div>
              <div className="mb-1 font-sans text-neutral-400">arguments</div>
              <pre className="whitespace-pre-wrap break-all text-neutral-600">
                {argsText}
              </pre>
            </div>
          )}
          {(resultText || isError) && (
            <div>
              <div className="mb-1 font-sans text-neutral-400">result</div>
              <pre
                className={`whitespace-pre-wrap break-all ${
                  isError ? "text-red-600" : "text-neutral-600"
                }`}
              >
                {resultText || "(no output)"}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  );
};
