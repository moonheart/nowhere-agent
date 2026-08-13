import { useMemo, type FC } from "react";
import type { TextMessagePartProps } from "@assistant-ui/react";
import DOMPurify from "dompurify";
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
  const html = useMemo(() => {
    const raw = marked.parse(text) as string;
    // marked (v5+) does NOT escape raw HTML — it passes tags straight
    // through — and its sanitize option was removed; model output (chat text,
    // tool results, subagent parts) is therefore untrusted and must be
    // stripped before it lands in dangerouslySetInnerHTML.
    return DOMPurify.sanitize(raw);
  }, [text]);
  return (
    <div
      className="markdown-body leading-relaxed"
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
};
