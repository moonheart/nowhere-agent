import { useMemo, type FC } from "react";
import type { TextMessagePartProps } from "@assistant-ui/react";
import { marked } from "marked";

// GFM so the model's tables/strikethrough/task-lists render; breaks so single
// newlines in streamed prose become <br> like the user expects in chat.
marked.setOptions({ gfm: true, breaks: true });

/**
 * Renders an assistant text part as Markdown. assistant-ui's default Text part
 * is a plain <span>, so **bold**, lists, code fences, and --- rules would show
 * as raw characters; this swaps in `marked` for the text part only (reasoning
 * and tool calls keep their own components).
 */
export const MarkdownText: FC<TextMessagePartProps> = ({ text }) => {
  const html = useMemo(() => marked.parse(text) as string, [text]);
  return (
    <div
      className="markdown-body leading-relaxed"
      // Content is model output rendered into the user's own session; marked
      // escapes raw HTML by default, so this stays inert.
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
};
