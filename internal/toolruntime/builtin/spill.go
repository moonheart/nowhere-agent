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
	// spillKeepHead is how much of an oversized result stays inline. It is below
	// spillCap so head + marker still fits the persistence cap and the retrieval
	// instructions in the marker are never themselves truncated.
	spillKeepHead = 18_000
	// spillDir is the reserved workspace scratch namespace for spilled results.
	// It is hidden from grep/glob (dropScratch) but readable with read_file, so
	// the model retrieves a truncated tail with the same paging tool it uses for
	// any file.
	spillDir = ".nowhere/tool-results"
)

// capAndSpill bounds a tool result for the model context (capability-gap T8). A
// result within spillCap is returned unchanged. An oversized result has its full
// payload written to a workspace scratch file and only its head returned inline,
// with a marker telling the model the path and the offset to read the rest with
// read_file. Spilling is best-effort: if the write fails the head is still
// returned, with a marker noting the tail was dropped (the pre-T8 behaviour), so
// a storage failure degrades gracefully rather than failing the tool call.
func capAndSpill(ctx context.Context, sb sandbox.Port, h sandbox.Handle, label, full string) string {
	if len(full) <= spillCap {
		return full
	}
	head := full[:spillKeepHead]
	path := spillPath(label, full)
	if err := sb.WriteFile(ctx, h, path, strings.NewReader(full)); err != nil {
		return head + fmt.Sprintf("\n\n… [output truncated: kept the first %d of %d bytes; the rest could not be saved (%v)]", spillKeepHead, len(full), err)
	}
	return head + fmt.Sprintf("\n\n… [output truncated: kept the first %d of %d bytes; full output saved to %s — read the rest with read_file(path=%q, offset=%d)]", spillKeepHead, len(full), path, path, spillKeepHead)
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
