// Package scheduler installs the periodic reconcile job.
//
// Hooks capture git provenance at the exact moment it matters, but they cannot
// cover a crash, a kill -9, or any session that ran before ccvault existed.
// The timer is the safety net that makes the index eventually complete.
package scheduler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Interval between reconcile runs, in seconds. Scans are cheap — unchanged
// transcripts are skipped on a size comparison — so this can be frequent.
const IntervalSeconds = 900

type Plan struct {
	Files   []string
	Enabled bool
	Hint    string
}

// Install writes the platform's scheduler units and activates them.
func Install(binary string, dryRun bool) (*Plan, error) {
	switch runtime.GOOS {
	case "linux":
		return installSystemd(binary, dryRun)
	case "darwin":
		return installLaunchd(binary, dryRun)
	default:
		return nil, fmt.Errorf("no scheduler support for %s; run `ccvault scan` from cron", runtime.GOOS)
	}
}

func installSystemd(binary string, dryRun bool) (*Plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")

	plan := &Plan{Files: []string{
		filepath.Join(dir, "ccvault-scan.service"),
		filepath.Join(dir, "ccvault-scan.timer"),
	}}
	if dryRun {
		return plan, nil
	}

	service, timer := RenderSystemd(binary)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(plan.Files[0], []byte(service), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(plan.Files[1], []byte(timer), 0o644); err != nil {
		return nil, err
	}
	return finishSystemd(plan)
}

// RenderSystemd builds the unit files. Exposed so their content can be checked
// without installing anything.
func RenderSystemd(binary string) (service, timer string) {
	service = fmt.Sprintf(`[Unit]
Description=ccvault: reconcile the Claude Code session index

[Service]
Type=oneshot
ExecStart=%s scan
Nice=10
IOSchedulingClass=idle
`, binary)

	timer = fmt.Sprintf(`[Unit]
Description=Run ccvault scan periodically

[Timer]
OnBootSec=2min
OnUnitActiveSec=%ds
# Catch up after the laptop was asleep or powered off.
Persistent=true

[Install]
WantedBy=timers.target
`, IntervalSeconds)
	return service, timer
}

func finishSystemd(plan *Plan) (*Plan, error) {
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		plan.Hint = "wrote units, but `systemctl --user daemon-reload` failed; run it yourself"
		return plan, nil
	}
	if err := run("systemctl", "--user", "enable", "--now", "ccvault-scan.timer"); err != nil {
		plan.Hint = "wrote units, but enabling the timer failed; run: systemctl --user enable --now ccvault-scan.timer"
		return plan, nil
	}
	plan.Enabled = true
	return plan, nil
}

func installLaunchd(binary string, dryRun bool) (*Plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")

	plan := &Plan{Files: []string{path}}
	if dryRun {
		return plan, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(RenderLaunchd(binary)), 0o644); err != nil {
		return nil, err
	}

	// bootout first so a re-install replaces the old definition cleanly.
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	_ = run("launchctl", "bootout", uid+"/"+LaunchdLabel)
	if err := run("launchctl", "bootstrap", uid, path); err != nil {
		plan.Hint = fmt.Sprintf("wrote the plist, but loading it failed; run: launchctl bootstrap %s %s", uid, path)
		return plan, nil
	}
	plan.Enabled = true
	return plan, nil
}

// LaunchdLabel identifies the macOS launch agent.
const LaunchdLabel = "com.sysn3rd.ccvault.scan"

// RenderLaunchd builds the launch agent plist. Exposed so its content can be
// checked from any platform.
func RenderLaunchd(binary string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>scan</string>
  </array>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>LowPriorityIO</key><true/>
  <key>Nice</key><integer>10</integer>
</dict>
</plist>
`, LaunchdLabel, binary, IntervalSeconds)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
