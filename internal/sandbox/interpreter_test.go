package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestOrderForHostWindowsPrefersRealPython: on Windows the Store `python3` shim
// sits on PATH but does nothing, so the `py` launcher and a real `python` must
// be probed first. Non-Python candidates keep their relative order.
func TestOrderForHostWindowsPrefersRealPython(t *testing.T) {
	got := orderForHost("windows", []string{"python3", "python", "py"})
	want := []string{"py", "python", "python3"}
	if !equalArgv(got, want) {
		t.Errorf("orderForHost windows = %v, want %v", got, want)
	}
}

// TestOrderForHostUnixKeepsPython3First: outside Windows there is no Store stub,
// so the conventional python3-first order is preserved verbatim.
func TestOrderForHostUnixKeepsPython3First(t *testing.T) {
	in := []string{"python3", "python", "py"}
	if got := orderForHost("linux", in); !equalArgv(got, in) {
		t.Errorf("orderForHost linux = %v, want unchanged %v", got, in)
	}
	if got := orderForHost("darwin", in); !equalArgv(got, in) {
		t.Errorf("orderForHost darwin = %v, want unchanged %v", got, in)
	}
}

// TestOrderForHostLeavesNonPythonAlone: shells and node are not reordered, and
// ranked Python names still sort ahead of them on Windows.
func TestOrderForHostLeavesNonPythonAlone(t *testing.T) {
	// sh/node have no Python rank; their relative order is kept.
	if got := orderForHost("windows", []string{"sh", "bash"}); !equalArgv(got, []string{"sh", "bash"}) {
		t.Errorf("shell order changed: %v", got)
	}
	if got := orderForHost("windows", []string{"node"}); !equalArgv(got, []string{"node"}) {
		t.Errorf("node order changed: %v", got)
	}
}

// makeFakeExe writes an executable named name into dir so exec.LookPath finds
// it when dir is prepended to PATH.
func makeFakeExe(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".cmd"
		if err := os.WriteFile(path, []byte("@echo off\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestResolveInterpreterPicksAvailable: the local backend probes the host and
// returns a candidate that exists. A fake interpreter on PATH makes the test
// hermetic instead of depending on sh/Git Bash being present on the host.
func TestResolveInterpreterPicksAvailable(t *testing.T) {
	bin := t.TempDir()
	makeFakeExe(t, bin, "sh")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := NewLocalPort(t.TempDir())
	if got := p.ResolveInterpreter([]string{"definitely-not-a-real-interp-xyz", "sh"}); got != "sh" {
		t.Errorf("ResolveInterpreter = %q, want sh (first available candidate)", got)
	}

	t.Setenv("PATH", bin)
	if got := p.ResolveInterpreter([]string{"definitely-not-a-real-interp-xyz"}); got != "" {
		t.Errorf("ResolveInterpreter with no usable candidate = %q, want \"\"", got)
	}
}

// TestDockerResolveInterpreterKeepsOrder: the docker backend cannot probe the
// image from the host, so it trusts the candidate order (python3 first).
func TestDockerResolveInterpreterKeepsOrder(t *testing.T) {
	p := &DockerPort{}
	in := []string{"python3", "python", "py"}
	if got := p.ResolveInterpreter(in); got != "python3" {
		t.Errorf("DockerPort.ResolveInterpreter = %q, want python3", got)
	}
	if got := p.ResolveInterpreter(nil); got != "" {
		t.Errorf("DockerPort.ResolveInterpreter(nil) = %q, want \"\"", got)
	}
}
