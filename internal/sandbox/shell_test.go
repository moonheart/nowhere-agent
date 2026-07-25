package sandbox

import (
	"runtime"
	"testing"
)

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestShellArgvUsesProvidedBash: an explicit bash path (SANDBOX_SHELL) is used
// verbatim, so the mapping is deterministic on any host regardless of GOOS.
func TestShellArgvUsesProvidedBash(t *testing.T) {
	argv, err := shellArgv("windows", `C:\Program Files\Git\bin\bash.exe`, "echo hi")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\Program Files\Git\bin\bash.exe`, "-c", "echo hi"}
	if !equalArgv(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
}

func TestShellArgvUnixExplicit(t *testing.T) {
	argv, err := shellArgv("linux", "/bin/bash", "ls -la")
	if err != nil {
		t.Fatal(err)
	}
	if !equalArgv(argv, []string{"/bin/bash", "-c", "ls -la"}) {
		t.Errorf("argv = %v", argv)
	}
}

// TestShellArgvAutoDetect: with no override, findShell must locate a shell on any
// real dev/CI host (Git Bash on the maintainer's Windows; sh/bash on unix). Skip
// only if the host genuinely has neither.
func TestShellArgvAutoDetect(t *testing.T) {
	if findShell(runtime.GOOS) == "" {
		t.Skip("no bash/sh on this host")
	}
	argv, err := shellArgv(runtime.GOOS, "", "true")
	if err != nil {
		t.Fatalf("auto-detect failed: %v", err)
	}
	if len(argv) != 3 || argv[1] != "-c" || argv[2] != "true" {
		t.Errorf("argv = %v", argv)
	}
}
