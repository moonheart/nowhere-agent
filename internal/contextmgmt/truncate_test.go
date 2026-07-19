package contextmgmt

import (
	"strings"
	"testing"

	"nowhere-agent/internal/provider"
)

func TestTruncateBlocksOversizedToolResult(t *testing.T) {
	big := strings.Repeat("x", MaxToolResultChars+500)
	blocks := []provider.Block{
		{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: big},
	}
	out := TruncateBlocksForPersistence(blocks)
	got := out[0].ToolContent
	if len(got) >= len(big) {
		t.Errorf("not truncated: len %d", len(got))
	}
	if !strings.Contains(got, "bytes truncated") {
		t.Errorf("missing truncation marker: %q", got[len(got)-40:])
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 100)) {
		t.Errorf("content prefix lost")
	}
	// Original slice not mutated.
	if blocks[0].ToolContent != big {
		t.Error("input mutated")
	}
}

func TestTruncateBlocksUnderCapUnchanged(t *testing.T) {
	blocks := []provider.Block{
		{Type: provider.BlockToolResult, ToolResultID: "tu1", ToolContent: "small"},
		{Type: provider.BlockText, Text: strings.Repeat("y", MaxToolResultChars+100)}, // text not capped
	}
	out := TruncateBlocksForPersistence(blocks)
	if out[0].ToolContent != "small" {
		t.Errorf("under-cap result changed: %q", out[0].ToolContent)
	}
	if len(out[1].Text) != MaxToolResultChars+100 {
		t.Errorf("non-tool-result block should be unchanged")
	}
}
