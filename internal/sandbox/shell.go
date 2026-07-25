package sandbox

import (
	"fmt"
	"os"
	"os/exec"
)

// shellArgv builds the argv that runs a POSIX shell script. The command contract
// is uniform bash/POSIX on every host: on Windows it uses Git Bash's bash.exe so
// a model's bash script behaves the same as on Linux/mac, sidestepping cmd.exe's
// different semantics. bashPath overrides detection when non-empty (from
// SANDBOX_SHELL); otherwise the shell is located per-GOOS by findShell.
//
// It is a pure function of (goos, bashPath, script) once bashPath is set, so the
// mapping is unit-testable on any host by passing an explicit bashPath.
func shellArgv(goos, bashPath, script string) ([]string, error) {
	sh := bashPath
	if sh == "" {
		sh = findShell(goos)
	}
	if sh == "" {
		return nil, fmt.Errorf("no shell found; set SANDBOX_SHELL to a bash executable (e.g. Git Bash's bash.exe)")
	}
	return []string{sh, "-c", script}, nil
}

// findShell locates a bash (or sh) executable for the host, or "" if none is
// found. It first honours a bash on PATH; on Windows it then probes the standard
// Git for Windows install locations; on unix it falls back to sh.
func findShell(goos string) string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if goos == "windows" {
		for _, c := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
				return c
			}
		}
		return ""
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	return ""
}
