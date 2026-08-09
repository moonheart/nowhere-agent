package builtin

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"nowhere-agent/internal/sandbox"
)

const (
	// spillCap is the size over which a tool result is spilled to the workspace
	// rather than returned whole. Kept equal to the persistence cap
	// (contextmgmt.MaxToolResultChars) so any result that survives capAndSpill
	// also persists intact — the tool layer never relies on that backstop.
	spillCap = 20_000
	// spillKeepHead is how much of an oversized result's START stays inline. It
	// shares the inline budget with the tail preview (spillKeepTail): head + tail
	// preview + marker must stay under spillCap, so the marker and preview are
	// never themselves truncated and the retrieval instructions in the marker
	// always survive.
	spillKeepHead = 15_000
	// spillKeepTail is how much of an oversized result's END is shown inline as
	// a tail preview, after a truncation marker. Command output and logs put
	// errors and final status at the tail, so the model sees both ends of a
	// large result without paging for it; the middle stays retrievable with
	// read_file at the marker's offset.
	spillKeepTail = 3_000
	// spillDir is the reserved workspace scratch namespace for spilled results.
	// It is hidden from grep/glob (dropScratch) but readable with read_file, so
	// the model retrieves a truncated tail with the same paging tool it uses for
	// any file.
	spillDir = ".nowhere/tool-results"
)

// capAndSpill bounds a tool result for the model context (capability-gap T8). A
// result within spillCap is returned unchanged. An oversized result has its full
// payload written to a workspace scratch file and its head plus a tail preview
// returned inline, with a marker telling the model the path and the offset to
// read the rest with read_file — the model sees both ends of a long output
// (headers at the start, errors/status at the end) without paging for either.
// Spilling is best-effort: if the write fails the head and tail preview are
// still returned, with a marker noting the middle could not be saved (the
// pre-T8 behaviour), so a storage failure degrades gracefully rather than
// failing the tool call.
func capAndSpill(ctx context.Context, sb sandbox.Port, h sandbox.Handle, label, full string) string {
	if len(full) <= spillCap {
		return full
	}
	// Cut the inline head and the tail preview on rune boundaries so both stay
	// valid UTF-8 and the offset handed to read_file lands on the tail's first
	// rune — otherwise a 3-byte rune straddling a cut would show as U+FFFD both
	// in the head and at the start of the retrieved tail (capability-gap T8).
	headCut := runeBoundaryAtOrBefore(full, spillKeepHead)
	head := full[:headCut]
	// The tail region always starts after the head: spillKeepHead + spillKeepTail
	// < spillCap <= len(full), so even both rune-boundary retreats cannot overlap.
	tailStart := runeBoundaryAtOrBefore(full, len(full)-spillKeepTail)
	tail := full[tailStart:]
	path := spillPath(label, full)
	if err := sb.WriteFile(ctx, h, path, strings.NewReader(full)); err != nil {
		return head + fmt.Sprintf(
			"\n\n… [output truncated: kept the first %d of %d bytes; the rest could not be saved (%v)]\n\n… [tail preview: last %d of %d bytes follows] …\n%s",
			headCut, len(full), err, len(tail), len(full), tail)
	}
	return head + fmt.Sprintf(
		"\n\n… [output truncated: kept the first %d of %d bytes; full output saved to %s — read the rest with read_file(path=%q, offset=%d)]\n\n… [tail preview: last %d of %d bytes follows] …\n%s",
		headCut, len(full), path, path, headCut, len(tail), len(full), tail)
}

// spillPath is the deterministic scratch path for a spilled result. The content
// hash keeps identical outputs at a single file (idempotent, and free of any
// clock/random input so it is reproducible in tests); the label namespaces the
// file for readability.
func spillPath(label, content string) string {
	hsh := fnv.New64a()
	_, _ = hsh.Write([]byte(content))
	return fmt.Sprintf("%s/%s-%x.txt", spillDir, label, hsh.Sum64())
}
