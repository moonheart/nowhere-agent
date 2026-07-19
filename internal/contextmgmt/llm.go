package contextmgmt

import (
	"context"
	"strings"

	"nowhere-agent/internal/provider"
)

// LLMCompressor implements Compressor by asking the model to summarize the
// dropped conversation. It reuses the loop's provider.Adapter and model, but
// sends NO tools — so the model can only emit text, the same guarantee Claude
// Code gets from its deny-all forked summarizer without the fork machinery
// (design D4). The summarize call honours ctx: a cancelled run aborts an
// in-flight summarize rather than finishing it in the background.
type LLMCompressor struct {
	adapter provider.Adapter
	model   string
	// MaxTokens bounds the summary length (the summary is small relative to the
	// context it replaces). Zero uses a default.
	MaxTokens int
}

// NewLLMCompressor creates a Compressor over the given adapter and model.
func NewLLMCompressor(adapter provider.Adapter, model string) *LLMCompressor {
	return &LLMCompressor{adapter: adapter, model: model, MaxTokens: 1024}
}

// Summarize renders the dropped messages into a transcript, asks the model for
// a structured summary, and returns it. The scratch <analysis> preamble some
// models emit is stripped; only the summary body is kept.
func (c *LLMCompressor) Summarize(ctx context.Context, dropped []provider.Message) (string, error) {
	req := provider.Request{
		Model:     c.model,
		System:    summarizeSystemPrompt,
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, renderTranscript(dropped))},
		Tools:     nil, // no tools: the summarizer can only emit text
		MaxTokens: c.MaxTokens,
	}
	events, err := c.adapter.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for ev := range events {
		switch ev.Type {
		case provider.EventBlockDelta:
			sb.WriteString(ev.Delta)
		case provider.EventError:
			return "", ev.Err
		}
	}
	return stripAnalysis(sb.String()), nil
}

// summarizeSystemPrompt is a condensed version of Claude Code's 9-section
// compact template (src/services/compact/prompt.ts), trimmed to what a working
// context handoff needs. It asks for an <analysis> scratch pass then a
// <summary>; formatCompactSummary strips the analysis, as stripAnalysis does.
const summarizeSystemPrompt = `You are summarizing a conversation so it can continue within a context budget. The dropped portion is replaced by your summary; the conversation continues from it.

First reason briefly inside <analysis> about what matters, then write the handoff inside <summary>. Cover, in order:
1. Primary intent — what the user is trying to accomplish.
2. Key technical concepts — frameworks, constraints, decisions made.
3. Files and code — paths and code that matter to the work.
4. Errors and fixes — problems hit and how they were resolved.
5. Current state and next step — where things stand and what to do next.

Be concise but complete: the reader has NO other record of this portion. Write in the conversation's language.`

// renderTranscript flattens dropped messages into a readable transcript for the
// summarizer. Thinking is included (it carries reasoning), tool exchanges are
// named, images are referenced by path.
func renderTranscript(msgs []provider.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		role := string(m.Role)
		for _, blk := range m.Content {
			switch blk.Type {
			case provider.BlockText:
				b.WriteString(role + ": " + blk.Text + "\n")
			case provider.BlockThinking:
				b.WriteString(role + " (thinking): " + blk.Thinking + "\n")
			case provider.BlockToolUse:
				b.WriteString(role + " [tool_use " + blk.ToolName + "]\n")
			case provider.BlockToolResult:
				b.WriteString("tool_result: " + blk.ToolContent + "\n")
			case provider.BlockImage:
				b.WriteString(role + " [image " + blk.ImagePath + "]\n")
			}
		}
	}
	return b.String()
}

// stripAnalysis removes a leading <analysis>...</analysis> scratch block and
// unwraps a <summary>...</summary> wrapper, returning just the summary body.
// If the model emitted neither tag, the text is returned as-is.
func stripAnalysis(s string) string {
	if i := strings.Index(s, "</analysis>"); i >= 0 {
		s = s[i+len("</analysis>"):]
	}
	if i := strings.Index(s, "<summary>"); i >= 0 {
		s = s[i+len("<summary>"):]
		if j := strings.Index(s, "</summary>"); j >= 0 {
			s = s[:j]
		}
	}
	return strings.TrimSpace(s)
}
