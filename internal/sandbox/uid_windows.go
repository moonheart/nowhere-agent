//go:build windows

package sandbox

// workspaceOwnerUID has no meaningful value on Windows: Docker Desktop runs
// containers in a Linux VM with its own uid space, and the host temp dir maps
// in through the VM. Returning false keeps the container's default user.
func workspaceOwnerUID(string) (int, bool) { return 0, false }
