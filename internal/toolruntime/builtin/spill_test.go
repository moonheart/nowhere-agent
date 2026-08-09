package builtin

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestCapAndSpillShowsTailPreview pins the head+tail upgrade: an oversized
// result keeps its head inline AND a tail preview (the last spillKeepTail
// bytes), so the model sees both ends of a long output without paging. The
// head ends exactly at the marker's offset and the preview comes after the
// truncation marker, so the two regions never overlap, and head + preview +
// marker still fit the persistence cap.
func TestCapAndSpillShowsTailPreview(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})
	full := strings.Repeat("A", spillKeepHead) + strings.Repeat("B", spillCap)

	got := capAndSpill(ctx, sb, h, "run_command", full)

	if !strings.HasSuffix(got, strings.Repeat("B", spillKeepTail)) {
		t.Errorf("result does not end with the tail preview (last %d bytes)", spillKeepTail)
	}
	if !strings.Contains(got, "tail preview") {
		t.Error("truncation marker missing the tail-preview notice")
	}
	off := markerOffset(t, got)
	if !strings.HasPrefix(got, full[:off]) {
		t.Errorf("head is not exactly the first %d bytes of the output", off)
	}
	if len(got) > contextmgmt.MaxToolResultChars {
		t.Errorf("capped result %d exceeds persistence cap %d", len(got), contextmgmt.MaxToolResultChars)
	}
}

// TestCapAndSpillTailPreviewSurvivesWriteFailure: even when the spill write
// fails, the tail preview still shows — it is cut from the in-memory output,
// not the file — so a storage failure only costs the retrievable middle, not
// the end of the output.
func TestCapAndSpillTailPreviewSurvivesWriteFailure(t *testing.T) {
	full := strings.Repeat("z", spillCap+1) + strings.Repeat("Y", spillKeepTail)
	got := capAndSpill(context.Background(), sandbox.NewMemPort(), sandbox.Handle{ID: "missing"}, "run_command", full)
	if !strings.Contains(got, "could not be saved") {
		t.Error("want a degraded marker on spill failure")
	}
	if !strings.HasSuffix(got, strings.Repeat("Y", spillKeepTail)) {
		t.Error("tail preview missing from the degraded result")
	}
}

// TestCapAndSpillTailPreviewRuneSafe: the tail preview is cut on a rune boundary
// too — a multi-byte rune straddling the tail cut must not surface as U+FFFD,
// and the preview must stay valid UTF-8 (capability-gap T8).
func TestCapAndSpillTailPreviewRuneSafe(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})
	// "x" + 汉*spillCap + "z": the tail cut target (len(spillKeepTail)) lands
	// mid-rune, forcing the retreat that a naive full[tail:] cut would get wrong.
	full := "x" + strings.Repeat("汉", spillCap) + "z"

	got := capAndSpill(ctx, sb, h, "run_command", full)

	if !utf8.ValidString(got) || strings.ContainsRune(got, '�') {
		t.Error("tail preview split a rune — result is not valid UTF-8")
	}
	if !strings.Contains(got, "tail preview") {
		t.Error("truncation marker missing the tail-preview notice")
	}
	// The preview ends with the output's final byte, intact and appearing once.
	if !strings.HasSuffix(got, "z") || strings.Count(got, "z") != 1 {
		t.Error("tail preview does not end with the intact final byte")
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

// TestCapAndSpillCutsHeadOnRuneBoundary pins the rune-safety fix for spilling: a
// 3-byte rune straddling spillKeepHead must not be split, so the inline head is
// valid UTF-8 and the offset in the marker lands on the tail's first rune (the
// exact byte read_file will be told to continue from).
func TestCapAndSpillCutsHeadOnRuneBoundary(t *testing.T) {
	ctx := context.Background()
	sb := sandbox.NewMemPort()
	h, _ := sb.Create(ctx, "s", sandbox.Options{})
	// A 1-byte prefix shifts every rune boundary off the (multiple-of-3)
	// spillKeepHead, so a naive full[:spillKeepHead] cut would split a 汉.
	full := "x" + strings.Repeat("汉", spillCap)

	got := capAndSpill(ctx, sb, h, "run_command", full)

	head, _, ok := strings.Cut(got, "\n\n… [output truncated")
	if !ok {
		t.Fatal("no truncation marker in spilled result")
	}
	if !utf8.ValidString(head) || strings.ContainsRune(head, '�') {
		t.Error("inline head split a rune — not valid UTF-8")
	}
	off := markerOffset(t, got)
	if head != full[:off] {
		t.Errorf("head is not exactly the first %d bytes of the output", off)
	}
	if !utf8.ValidString(full[off:]) {
		t.Error("tail at the marker offset is not valid UTF-8 — the cut split a rune")
	}
	// The cut retreated a little from spillKeepHead to reach the boundary.
	if d := spillKeepHead - off; d < 1 || d > 3 {
		t.Errorf("cut offset %d not just below spillKeepHead %d (retreat %d)", off, spillKeepHead, d)
	}
}
