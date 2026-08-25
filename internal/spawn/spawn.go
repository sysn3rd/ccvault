// Package spawn opens a new terminal window running a command.
//
// This exists because continuing a session in place is wrong when the picker
// was itself opened as a floating scratch window: replacing that process leaves
// the resumed session trapped in a small floating terminal that still matches
// the picker's window rule. A new window gets its own identity.
package spawn

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// AppID keeps the resumed session from inheriting the picker's window rules.
const AppID = "org.omarchy.claude"

// Terminal describes how to open a window on this machine.
type Terminal struct {
	Name string
	Args func(appID string) []string
}

// candidates are tried in order. xdg-terminal-exec is first because it is the
// freedesktop entry point Omarchy itself uses, so it honours whichever terminal
// the user actually chose.
var candidates = []Terminal{
	{"xdg-terminal-exec", func(id string) []string { return []string{"--app-id=" + id, "-e"} }},
	{"foot", func(id string) []string { return []string{"--app-id=" + id, "-e"} }},
	{"ghostty", func(id string) []string { return []string{"--class=" + id, "-e"} }},
	{"kitty", func(id string) []string { return []string{"--class=" + id, "--"} }},
	{"alacritty", func(id string) []string { return []string{"--class=" + id, "-e"} }},
	{"wezterm", func(id string) []string { return []string{"start", "--class=" + id, "--"} }},
	{"xterm", func(id string) []string { return []string{"-class", id, "-e"} }},
}

// Command builds the terminal invocation. override comes from the settings file
// and wins outright; it is split on spaces and the command is appended.
func Command(override string, dir string, argv []string) (*exec.Cmd, error) {
	if runtime.GOOS == "darwin" && override == "" {
		return macCommand(dir, argv)
	}

	if override != "" {
		fields := strings.Fields(override)
		if len(fields) == 0 {
			return nil, fmt.Errorf("picker.terminal is set but empty")
		}
		cmd := exec.Command(fields[0], append(fields[1:], argv...)...)
		cmd.Dir = dir
		return cmd, nil
	}

	for _, c := range candidates {
		path, err := exec.LookPath(c.Name)
		if err != nil {
			continue
		}
		args := append(c.Args(AppID), argv...)
		cmd := exec.Command(path, args...)
		// Terminals inherit the working directory of whatever launched them,
		// which is how the new window lands in the session's directory without
		// every terminal needing its own flag for it.
		cmd.Dir = dir
		return cmd, nil
	}
	return nil, fmt.Errorf("no terminal found; set picker.terminal in the settings file")
}

// macCommand drives Terminal.app, which has no argv form that also sets a
// working directory, so the shell command carries both.
func macCommand(dir string, argv []string) (*exec.Cmd, error) {
	quoted := make([]string, 0, len(argv))
	for _, a := range argv {
		quoted = append(quoted, shellQuote(a))
	}
	script := fmt.Sprintf("cd %s && exec %s", shellQuote(dir), strings.Join(quoted, " "))
	osa := fmt.Sprintf("tell application \"Terminal\"\nactivate\ndo script %s\nend tell", appleQuote(script))
	return exec.Command("osascript", "-e", osa), nil
}

// Detach starts the command fully detached, so the picker can exit immediately
// without taking the new window down with it.
func Detach(cmd *exec.Cmd) error {
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Env = os.Environ()
	setsid(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func appleQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
