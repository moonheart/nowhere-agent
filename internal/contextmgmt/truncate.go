package contextmgmt

import (
	"fmt"

	"nowhere-agent/internal/provider"
)

// MaxToolResultChars bounds a single persisted tool_result block's text.
// Oversized results are truncated before persistence (persist-raw-messages D6)
// so a huge file read or command output cannot blow up a messages.content
// JSONB row. This is now a last-resort backstop: the tool layer keeps its own
// results under this cap — read_file pages large files and run_command spills
// oversized output to the workspace, retrievable with read_file (builtin's
// capAndSpill, capability-gap T8) — so a tail is truncated here only for a tool
// that does neither.
const MaxToolResultChars = 20_000

// TruncationMarker is appended to truncated tool_result content so the model
// (and any reader) knows the payload was cut.
const truncationMarker = "\n\n... [%d bytes truncated] ..."

// TruncateBlocksForPersistence returns a copy of blocks with any oversized
// tool_result text truncated to MaxToolResultChars and a marker appended. Other
// block types are returned unchanged. The input slice is not mutated.
func TruncateBlocksForPersistence(blocks []provider.Block) []provider.Block {
	out := make([]provider.Block, len(blocks))
	copy(out, blocks)
	for i := range out {
		b := &out[i]
		if b.Type != provider.BlockToolResult {
			continue
		}
		if len(b.ToolContent) <= MaxToolResultChars {
			continue
		}
		omitted := len(b.ToolContent) - MaxToolResultChars
		b.ToolContent = b.ToolContent[:MaxToolResultChars] + fmt.Sprintf(truncationMarker, omitted)
	}
	return out
}
