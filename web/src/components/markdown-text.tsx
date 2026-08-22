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
    let raw = marked.parse(text) as string;
    // External links open in a new window.
    raw = raw.replace(/<a /g, '<a target="_blank" rel="noopener noreferrer" ');
    // marked (v5+) does NOT escape raw HTML — it passes tags straight
    // through — and its sanitize option was removed; model output (chat text,
    // tool results, subagent parts) is therefore untrusted and must be
    // stripped before it lands in dangerouslySetInnerHTML. style is forbidden
    // outright: CSS is a paste-up channel for hidden/overlapping content.
    // Explicitly allow target (DOMPurify strips it by default without rel).
    return DOMPurify.sanitize(raw, { FORBID_ATTR: ["style"], ADD_ATTR: ["target"] });
  }, [text]);
  return (
    <div
      className="markdown-body leading-relaxed"
      dangerouslySetInnerHTML={{ __html: html }}
    />
  );
};
