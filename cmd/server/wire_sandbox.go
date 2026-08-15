package main

import (
	"context"
	"fmt"
	"time"

	"nowhere-agent/internal/sandbox"
	"nowhere-agent/internal/settings"
)

// wire_sandbox.go — the sandbox backend (local/docker/off) for the built-in
// file/exec tools, the lifecycle reaper, and the exec-enabled predicate.
// Extracted verbatim from run() (see deps.go).

func (d *serverDeps) wireSandbox(ctx context.Context) error {
	cfg, log := d.cfg, d.log

	// Sandbox for built-in tools (file-tools): a per-session sandbox Manager
	// over the configured backend. The tool binder (below) ensures the session's
	// sandbox and registers its file tools for each run. "off" leaves tools
	// unregistered (pre-file-tools behaviour).
	// The workspace root hosts per-session sandbox workspaces under
	// <root>/<sessionID> for BOTH backends (the same convention the ImageStore
	// and workspace.Store use): the local backend confines files to it, the
	// docker backend bind-mounts it into the container.
	d.wsRoot = cfg.Sandbox.WorkspaceDir
	if d.wsRoot == "" {
		d.wsRoot = cfg.Workspace.Dir
	}
	switch cfg.Sandbox.Backend {
	case "local":
		if d.wsRoot == "" {
			return fmt.Errorf("SANDBOX_BACKEND=local requires SANDBOX_WORKSPACE_DIR or WORKSPACE_DIR")
		}
		d.sandboxPort = sandbox.NewLocalPort(d.wsRoot).WithShell(cfg.Sandbox.Shell)
		log.Info("sandbox backend: local fs", "root", d.wsRoot)
	case "docker":
		if d.wsRoot == "" {
			return fmt.Errorf("SANDBOX_BACKEND=docker requires SANDBOX_WORKSPACE_DIR or WORKSPACE_DIR")
		}
		var dockerOpts []sandbox.DockerOption
		if cfg.Sandbox.Image != "" {
			dockerOpts = append(dockerOpts, sandbox.WithImage(cfg.Sandbox.Image))
		}
		dp, err := sandbox.NewDockerPort(dockerOpts...)
		if err != nil {
			return fmt.Errorf("docker sandbox: %w", err)
		}
		d.sandboxPort = dp
		log.Info("sandbox backend: docker", "root", d.wsRoot, "image", dp.Image())
	case "off", "":
		log.Info("sandbox backend: off (no built-in tools)")
	default:
		return fmt.Errorf("unknown SANDBOX_BACKEND %q", cfg.Sandbox.Backend)
	}
	if d.sandboxPort != nil {
		d.sandboxMgr = sandbox.NewManager(d.sandboxPort)
	}
	// Sandbox lifecycle reaper (resource leak D3): the Manager's sweep destroys
	// sandboxes whose deferred-stop deadline passed. Registered for the DOCKER
	// backend only: the local backend's Destroy removes the session workspace
	// dir (<root>/<sessionID>), which overlaps the ImageStore's per-session
	// image dir under the same root — a sweep there would delete a retention-
	// window session's images early. The docker Destroy only stops/removes the
	// container (workspace bind-mount files are untouched), so it is safe.
	if cfg.Sandbox.Backend == "docker" && d.sandboxMgr != nil {
		hourlySweep(ctx, log, "sandbox", func() error {
			destroyed, err := d.sandboxMgr.Sweep(ctx, time.Now())
			if err != nil {
				return err
			}
			if len(destroyed) > 0 {
				log.Info("sandbox sweep destroyed ended-session sandboxes", "sessions", destroyed)
			}
			return nil
		})
	}
	return nil
}

// execEnabledFor reports whether run_command is available: the docker backend
// always offers it (the command is contained in the Linux container); the local
// backend only when explicitly enabled via SANDBOX_LOCAL_EXEC (runtime-settable
// from the admin console as sandbox_local_exec), since there it runs on the
// host. Resolved per session inside the tool registry, so the switch applies
// to the next run.
func (d *serverDeps) execEnabledFor() bool {
	if d.cfg.Sandbox.Backend == "docker" {
		return true
	}
	return d.cfg.Sandbox.Backend == "local" && d.settings.Bool(settings.KeySandboxLocalExec)
}
