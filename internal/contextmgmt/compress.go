// Package contextmgmt implements context-management (design D11): online
// compression of the in-context short-term memory so a session stays within
// the model's context budget. It is distinct from offline dreaming: it governs
// only the current session's context and never writes long-term memory.
package contextmgmt

import (
	"context"
	"encoding/json"
	"strings"

	"nowhere-agent/internal/provider"
)

// Policy configures when and how to compress.
type Policy struct {
	// MaxTokens is the context budget for the window.
	MaxTokens int
	// Threshold is the fraction of MaxTokens at which compression triggers
	// (e.g. 0.8). Compression runs when estimated tokens exceed it.
	Threshold float64
	// KeepRecent is how many recent rounds are always preserved verbatim. A
	// round is an assistant message plus the tool_result answers to its tool_use
	// blocks (design D2): keeping whole rounds — not a raw message count — is
	// what guarantees a tool_use is never severed from its tool_result.
	KeepRecent int
}

// DefaultPolicy returns a sensible default.
func DefaultPolicy() Policy {
	return Policy{MaxTokens: 100_000, Threshold: 0.8, KeepRecent: 6}
}

// estimateTokens approximates token count for a message set (~4 chars/token).
func estimateTokens(msgs []provider.Message) int {
	return contentBytes(msgs) / 4
}

// imageRefBytes is the conservative byte estimate for a path-only image block
// whose payload size is unknown until materialization (~1000 tokens).
const imageRefBytes = 4000

// contentBytes approximates the serialized byte size of a message set. Tool
// input VALUES are counted (a write_file's whole file body lives there) and
// images contribute their base64 payload once materialized, or a flat
// estimate while they are still path references. Doubling as the compression
// cache's fingerprint, it is deterministic (map iteration order-independent).
func contentBytes(msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			total += len(b.Text) + len(b.Thinking) + len(b.ToolContent) + len(b.ToolName) + len(b.ArgsError)
			for k, v := range b.ToolInput {
				total += len(k) + 8
				if raw, err := json.Marshal(v); err == nil {
					total += len(raw)
				}
			}
			if b.Type == provider.BlockImage {
				if len(b.ImageData) > 0 {
					total += len(b.ImageData)
				} else {
					total += imageRefBytes
				}
			}
		}
	}
	return total
}

// EstimateTokens exposes the approximate token count of a message set, so the
// agent loop can size its working view against the model's context window
// (design D5) using the same heuristic compression triggers on.
func EstimateTokens(msgs []provider.Message) int {
	return estimateTokens(msgs)
}

// ShouldCompress reports whether the history exceeds the trigger threshold.
func ShouldCompress(history []provider.Message, p Policy) bool {
	if p.MaxTokens <= 0 || p.Threshold <= 0 {
		return false
	}
	return estimateTokens(history) > int(float64(p.MaxTokens)*p.Threshold)
}

// SummaryPrefix marks the text block carrying a compression summary, so the
// overflow fallback can recognize (and preserve) it.
const SummaryPrefix = "[Earlier conversation summarized]\n"

// SummaryMessage builds the user-role message that replaces the summarized
// portion of the conversation.
func SummaryMessage(summary string) provider.Message {
	return provider.TextMessage(provider.RoleUser, SummaryPrefix+summary)
}

// IsSummary reports whether m is a compression-summary message.
func IsSummary(m provider.Message) bool {
	return m.Role == provider.RoleUser && len(m.Content) > 0 &&
		m.Content[0].Type == provider.BlockText && strings.HasPrefix(m.Content[0].Text, SummaryPrefix)
}

// CompressionCache carries one run's compression summary across loop
// iterations. The working view is append-only within a run, so a cached
// summary stays valid as long as the region it covers is unchanged; the byte
// fingerprint guards against same-length-different-content collisions.
type CompressionCache struct {
	// Covered is how many leading view messages Summary replaces.
	Covered int
	// CoveredBytes is the contentBytes fingerprint of the covered region.
	CoveredBytes int
	// Summary is the summarized text for the covered region.
	Summary string
}

// Compressor summarizes dropped history. Implemented by an LLM caller or a
// simple heuristic. It must NOT write to long-term memory. The ctx honours
// cancellation: a cancelled run aborts an in-flight summarize.
type Compressor interface {
	Summarize(ctx context.Context, dropped []provider.Message) (string, error)
}

// Compress reduces history when over threshold: older rounds are summarized
// into a single system-anchored summary message and the most recent KeepRecent
// rounds are kept verbatim (sliding window over rounds, not messages). The
// result is passed through EnsurePairing so the split can never leave an
// unpaired tool_use/tool_result. If under threshold, history is returned
// unchanged.
func Compress(ctx context.Context, history []provider.Message, p Policy, c Compressor) ([]provider.Message, error) {
	return CompressWithCache(ctx, history, p, c, nil)
}

// CompressWithCache is Compress with a per-run cache. Within a run the view
// only grows by appends, so:
//
//   - If the cached summary plus everything appended since still fits the full
//     budget, it is reused verbatim — no summarizer call and a byte-stable
//     prompt prefix across iterations (hysteresis: compression triggers at
//     Threshold but is re-done only when the tail threatens the budget).
//   - Otherwise the summary is extended incrementally: the summarizer receives
//     the previous summary plus only the newly dropped rounds, never re-reading
//     the whole dropped prefix (O(new) per call instead of O(total)).
//
// After summarizing, the result is rechecked against the budget; if the kept
// rounds alone still exceed it, oldest rounds are hard-dropped (summary
// preserved) until the view fits or nothing safe remains to drop.
func CompressWithCache(ctx context.Context, history []provider.Message, p Policy, c Compressor, cache *CompressionCache) ([]provider.Message, error) {
	if !ShouldCompress(history, p) {
		return history, nil
	}
	keep := p.KeepRecent
	if keep < 0 {
		keep = 0
	}

	rounds := groupRounds(history)
	if keep >= len(rounds) {
		return history, nil
	}

	// Split on a round boundary: everything before the first kept round is
	// summarized, the kept rounds pass through verbatim.
	splitIdx := rounds[len(rounds)-keep].start
	if keep == 0 {
		splitIdx = len(history)
	}

	cacheValid := cache != nil && cache.Covered > 0 && cache.Covered <= splitIdx &&
		cache.CoveredBytes == contentBytes(history[:cache.Covered])

	// Hysteresis reuse: cached summary + everything appended since fits the
	// full budget — send it unchanged.
	if cacheValid {
		candidate := make([]provider.Message, 0, len(history)-cache.Covered+1)
		candidate = append(candidate, SummaryMessage(cache.Summary))
		candidate = append(candidate, history[cache.Covered:]...)
		candidate = EnsurePairing(candidate)
		if estimateTokens(candidate) <= p.MaxTokens {
			return candidate, nil
		}
	}

	// Incremental extension: summarize the previous summary plus only the
	// rounds dropped since, instead of re-reading the whole dropped prefix.
	toSummarize := history[:splitIdx]
	if cacheValid && cache.Covered < splitIdx {
		toSummarize = make([]provider.Message, 0, splitIdx-cache.Covered+1)
		toSummarize = append(toSummarize, SummaryMessage(cache.Summary))
		toSummarize = append(toSummarize, history[cache.Covered:splitIdx]...)
	}

	summary, err := c.Summarize(ctx, toSummarize)
	if err != nil {
		return nil, err
	}
	if cache != nil {
		cache.Covered = splitIdx
		cache.CoveredBytes = contentBytes(history[:splitIdx])
		cache.Summary = summary
	}

	out := make([]provider.Message, 0, len(history)-splitIdx+1)
	out = append(out, SummaryMessage(summary))
	out = append(out, history[splitIdx:]...)
	out = EnsurePairing(out)

	// Post-check: the kept rounds alone may still exceed the budget (a huge
	// recent tool result). Hard-drop oldest rounds — summary preserved — until
	// the view fits or only one round remains.
	for ShouldCompress(out, p) {
		shrunk, ok := DropOldestRoundPreservingSummary(out)
		if !ok {
			break
		}
		out = shrunk
	}
	return out, nil
}
