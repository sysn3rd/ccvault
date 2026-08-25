// Package hooks installs ccvault's capture hooks into Claude Code's settings.
//
// This is the only place ccvault writes inside ~/.claude. It merges: existing
// settings keys and existing hooks on the same events are preserved, and
// re-running is a no-op.
package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Events we attach to. SessionStart captures git provenance the moment work
// begins; SessionEnd captures it again, plus whatever the session left dirty.
var Events = []string{"SessionStart", "SessionEnd"}

type Command struct {
	Type          string  `json:"type"`
	Command       string  `json:"command"`
	Timeout       float64 `json:"timeout,omitempty"`
	Async         bool    `json:"async,omitempty"`
	StatusMessage string  `json:"statusMessage,omitempty"`
}

type Group struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

// Plan describes what Install would do, so callers can show it before writing.
type Plan struct {
	SettingsPath string
	Added        []string // events that gained a hook
	AlreadyOK    []string // events that already had ours
	BackupPath   string
}

// commandFor builds the hook command line for an event.
//
// SessionStart runs async: nothing downstream depends on it finishing, and a
// background hook cannot delay session startup at all. SessionEnd must run
// synchronously — an async hook racing process exit may never complete — but is
// capped by a short timeout.
func commandFor(binary, event string) Command {
	c := Command{
		Type:    "command",
		Command: fmt.Sprintf("%s capture --hook", shellQuote(binary)),
		Timeout: 10,
	}
	if event == "SessionStart" {
		c.Async = true
	}
	return c
}

// Install merges ccvault's hooks into settingsPath. It is idempotent: a hook
// whose command already matches is left alone.
func Install(settingsPath, binary string, dryRun bool) (*Plan, error) {
	plan := &Plan{SettingsPath: settingsPath}

	// Preserve every key we do not understand, byte for byte.
	settings := map[string]json.RawMessage{}
	raw, err := os.ReadFile(settingsPath)
	switch {
	case err == nil:
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &settings); err != nil {
				return nil, fmt.Errorf("%s is not valid JSON (a malformed settings file disables every setting in it): %w", settingsPath, err)
			}
		}
	case os.IsNotExist(err):
		// fresh install
	default:
		return nil, err
	}

	allHooks := map[string][]Group{}
	if h, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(h, &allHooks); err != nil {
			return nil, fmt.Errorf("existing hooks block is not in the expected shape: %w", err)
		}
	}

	for _, event := range Events {
		want := commandFor(binary, event)
		if hasCommand(allHooks[event], want.Command) {
			plan.AlreadyOK = append(plan.AlreadyOK, event)
			continue
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			return nil, err
		}
		// No matcher: match every source (startup, resume, clear, compact).
		allHooks[event] = append(allHooks[event], Group{Hooks: []json.RawMessage{encoded}})
		plan.Added = append(plan.Added, event)
	}

	if len(plan.Added) == 0 || dryRun {
		return plan, nil
	}

	encodedHooks, err := json.Marshal(allHooks)
	if err != nil {
		return nil, err
	}
	settings["hooks"] = encodedHooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')

	if len(raw) > 0 {
		plan.BackupPath = fmt.Sprintf("%s.ccvault-backup.%d", settingsPath, time.Now().Unix())
		if err := os.WriteFile(plan.BackupPath, raw, 0o600); err != nil {
			return nil, err
		}
	}
	if err := writeAtomic(settingsPath, out); err != nil {
		return nil, err
	}
	return plan, nil
}

func hasCommand(groups []Group, command string) bool {
	for _, g := range groups {
		for _, h := range g.Hooks {
			var c Command
			if err := json.Unmarshal(h, &c); err != nil {
				continue
			}
			if c.Command == command {
				return true
			}
		}
	}
	return false
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ccvault-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// shellQuote guards against spaces in the install path; the hook command runs
// through a shell.
func shellQuote(s string) string {
	for _, r := range s {
		if r == ' ' || r == '\'' || r == '"' || r == '$' || r == '`' {
			return "'" + escapeSingle(s) + "'"
		}
	}
	return s
}

func escapeSingle(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
