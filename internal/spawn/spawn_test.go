package spawn

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolatePATH points PATH at a directory containing only the given fake
// executables, so terminal detection is deterministic rather than dependent on
// whatever the machine running the tests happens to have installed.
func isolatePATH(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

// The new window must open in the session's directory. Terminals inherit the
// working directory of whatever launched them, which is why this is set on the
// command rather than passed as a per-terminal flag.
func TestCommandRunsInTheSessionDirectory(t *testing.T) {
	dir := isolatePATH(t, "foot")
	work := t.TempDir()

	cmd, err := detectCommand(work, []string{"claude", "--resume", "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Dir != work {
		t.Errorf("Dir = %q, want %q", cmd.Dir, work)
	}
	if filepath.Dir(cmd.Path) != dir {
		t.Errorf("picked %q, which is not the isolated terminal", cmd.Path)
	}
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"claude", "--resume", "abc123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %v missing %q", cmd.Args, want)
		}
	}
}

// The resumed session must not inherit the picker's window rules, which is what
// the app id is for — without it a floating scratch picker hands its geometry
// to the Claude session that replaces it.
func TestCommandSetsADistinctAppID(t *testing.T) {
	isolatePATH(t, "foot")
	cmd, err := detectCommand(t.TempDir(), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), AppID) {
		t.Errorf("args %v do not carry the app id %q", cmd.Args, AppID)
	}
	if AppID == "org.omarchy.ccvault" {
		t.Error("the resumed session must not reuse the picker's app id")
	}
}

// xdg-terminal-exec is the freedesktop entry point, so it wins when present:
// it honours whichever terminal the user actually chose.
func TestCommandPrefersTheFreedesktopEntryPoint(t *testing.T) {
	isolatePATH(t, "xterm", "foot", "xdg-terminal-exec")
	cmd, err := detectCommand(t.TempDir(), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "xdg-terminal-exec" {
		t.Errorf("chose %q, want xdg-terminal-exec", filepath.Base(cmd.Path))
	}
}

func TestCommandFallsBackThroughTheCandidates(t *testing.T) {
	isolatePATH(t, "xterm")
	cmd, err := detectCommand(t.TempDir(), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "xterm" {
		t.Errorf("chose %q, want the only available terminal", filepath.Base(cmd.Path))
	}
}

// A configured terminal wins outright, since the point of the setting is to
// override detection.
func TestConfiguredTerminalOverridesDetection(t *testing.T) {
	isolatePATH(t, "foot")
	work := t.TempDir()

	cmd, err := Command("myterm --title ccvault -e", work, []string{"claude", "--resume", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "myterm" {
		t.Errorf("Path = %q, want the configured terminal", cmd.Path)
	}
	want := []string{"myterm", "--title", "ccvault", "-e", "claude", "--resume", "x"}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != work {
		t.Errorf("Dir = %q, want %q", cmd.Dir, work)
	}
}

// The dispatcher must reach the right builder for the platform it is on.
func TestCommandDispatchesByPlatform(t *testing.T) {
	isolatePATH(t, "foot", "osascript")
	cmd, err := Command("", t.TempDir(), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	got := filepath.Base(cmd.Path)
	want := "foot"
	if runtime.GOOS == "darwin" {
		want = "osascript"
	}
	if got != want {
		t.Errorf("on %s Command chose %q, want %q", runtime.GOOS, got, want)
	}
}

// With no terminal at all, the error has to say what to do about it.
func TestNoTerminalGivesAnActionableError(t *testing.T) {
	isolatePATH(t) // nothing installed
	_, err := detectCommand(t.TempDir(), []string{"claude"})
	if err == nil {
		t.Fatal("expected an error when no terminal exists")
	}
	if !strings.Contains(err.Error(), "picker.terminal") {
		t.Errorf("error should name the setting that fixes it: %v", err)
	}
}

func TestEmptyConfiguredTerminalIsRejected(t *testing.T) {
	isolatePATH(t, "foot")
	if _, err := Command("   ", t.TempDir(), []string{"claude"}); err == nil {
		t.Error("a blank-but-present terminal setting should be an error, not silently ignored")
	}
}

// The macOS path builds an AppleScript string, so both layers of quoting have
// to survive paths and arguments containing quotes.
func TestMacCommandQuotesSafely(t *testing.T) {
	cmd, err := macCommand("/Users/me/my project", []string{"claude", "--resume", "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(cmd.Path) != "osascript" {
		t.Errorf("Path = %q, want osascript", cmd.Path)
	}
	script := strings.Join(cmd.Args, " ")
	if !strings.Contains(script, "my project") {
		t.Errorf("the directory was lost: %q", script)
	}
	if !strings.Contains(script, "claude") {
		t.Errorf("the command was lost: %q", script)
	}

	// A single quote in a path passes through two layers of escaping: shellQuote
	// turns it into '\'' for the shell, then appleQuote doubles the backslash so
	// AppleScript reproduces it verbatim. The final string therefore carries
	// '\\'' — anything less would let the quote close the shell string early.
	tricky, err := macCommand("/Users/me/it's here", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	trickyScript := strings.Join(tricky.Args, " ")
	if !strings.Contains(trickyScript, `'\\''`) {
		t.Errorf("a quote in the path was not escaped for both layers: %q", trickyScript)
	}
	if !strings.Contains(trickyScript, "it") || !strings.Contains(trickyScript, "s here") {
		t.Errorf("the path did not survive escaping: %q", trickyScript)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":      "'plain'",
		"with space": "'with space'",
		"it's":       `'it'\''s'`,
		"$(whoami)":  "'$(whoami)'",
		"back`tick":  "'back`tick'",
		";rm -rf /":  "';rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppleQuote(t *testing.T) {
	if got := appleQuote(`say "hi"`); got != `"say \"hi\""` {
		t.Errorf("appleQuote = %q", got)
	}
	if got := appleQuote(`back\slash`); got != `"back\\slash"` {
		t.Errorf("appleQuote = %q", got)
	}
}
