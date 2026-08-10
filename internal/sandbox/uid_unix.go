//go:build !windows

package sandbox

import (
	"os"
	"syscall"
)

// workspaceOwnerUID returns the numeric owner uid of a host path, so a Docker
// container can run as the same user that owns its bind-mounted workspace.
// Unix-only: on Windows the container lives in a Linux VM with its own uid
// space, where the mapping is meaningless.
func workspaceOwnerUID(path string) (int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(sys.Uid), true
}
