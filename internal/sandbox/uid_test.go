//go:build !windows

package sandbox

import (
	"os"
	"testing"
)

// TestWorkspaceOwnerUID verifies the uid helper reports the owner of a host
// path — the value the docker backend matches its container user to. A temp
// dir created by this process is owned by the process uid.
func TestWorkspaceOwnerUID(t *testing.T) {
	dir := t.TempDir()
	uid, ok := workspaceOwnerUID(dir)
	if !ok {
		t.Fatal("workspaceOwnerUID returned ok=false for an existing dir")
	}
	if uid != os.Getuid() {
		t.Errorf("uid = %d, want %d (process owner)", uid, os.Getuid())
	}

	// A missing path has no owner to map.
	if _, ok := workspaceOwnerUID(dir + "-nope"); ok {
		t.Error("workspaceOwnerUID ok=true for a missing path")
	}
}
