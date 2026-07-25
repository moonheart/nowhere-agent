package builtin

import (
	"context"
	"strings"
	"testing"

	"nowhere-agent/internal/contextmgmt"
	"nowhere-agent/internal/sandbox"
)

// TestCapAndSpillPassesThroughSmall: a result within the cap is returned
// unchanged and nothing is written to the workspace.
func TestCapAndSpillPassesThroughSmall(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})

	in := "small output"
	if got := capAndSpill(ctx, sb, h, "run_command", in); got != in {
		t.Errorf("small result should pass through unchanged, got %q", got)
	}
	if files, _ := sb.Walk(ctx, h, "."); len(files) != 0 {
		t.Errorf("no spill expected for a small result, got %v", files)
	}
}

// TestCapAndSpillWritesTailAndRetrieves pins T8: an oversized result keeps its
// head inline with a retrieval marker, stays under the persistence cap, and its
// full payload is written to a scratch file that read-back returns verbatim.
func TestCapAndSpillWritesTailAndRetrieves(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})
	full := strings.Repeat("A", spillKeepHead) + strings.Repeat("B", 6000)

	got := capAndSpill(ctx, sb, h, "run_command", full)

	if !strings.HasPrefix(got, strings.Repeat("A", spillKeepHead)) {
		t.Error("kept head is not the first spillKeepHead bytes of the output")
	}
	if !strings.Contains(got, "read_file") || !strings.Contains(got, "offset=") {
		t.Errorf("marker missing retrieval instructions: %q", got[spillKeepHead:])
	}
	// head + marker must fit the persistence cap, or the marker itself would be
	// truncated on persistence and the model would lose the retrieval path.
	if len(got) > contextmgmt.MaxToolResultChars {
		t.Errorf("capped result %d exceeds persistence cap %d", len(got), contextmgmt.MaxToolResultChars)
	}

	if stored := readFile(t, sb, h, spillPath("run_command", full)); stored != full {
		t.Errorf("spill file len=%d, want the full %d bytes", len(stored), len(full))
	}
}

// TestCapAndSpillDegradesWhenWriteFails: a spill-store failure must not fail the
// tool — the head is still returned, with a "could not be saved" marker.
func TestCapAndSpillDegradesWhenWriteFails(t *testing.T) {
	full := strings.Repeat("z", spillCap+1)
	// A bare MemPort with no created sandbox: WriteFile returns "not found".
	got := capAndSpill(context.Background(), sandbox.NewMemPort(), sandbox.Handle{ID: "missing"}, "run_command", full)
	if !strings.HasPrefix(got, strings.Repeat("z", spillKeepHead)) {
		t.Error("head not returned on spill failure")
	}
	if !strings.Contains(got, "could not be saved") {
		t.Errorf("want a degraded marker, got tail %q", got[spillKeepHead:])
	}
}

// TestSpillPathStableForSameContent: content-hash naming is deterministic
// (idempotent for identical output) and lives under the scratch namespace.
func TestSpillPathStableForSameContent(t *testing.T) {
	a := spillPath("run_command", "same")
	b := spillPath("run_command", "same")
	c := spillPath("run_command", "different")
	if a != b {
		t.Errorf("same content must map to the same path: %q vs %q", a, b)
	}
	if a == c {
		t.Error("different content must map to different paths")
	}
	if !strings.HasPrefix(a, spillDir+"/") {
		t.Errorf("spill path not under the scratch dir: %q", a)
	}
}
