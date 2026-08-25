package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func settingsWith(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("settings.json is not valid JSON after install: %v", err)
	}
	return m
}

// A malformed settings.json silently disables every setting in it, so existing
// keys must survive byte-for-byte in meaning.
func TestInstallPreservesExistingSettings(t *testing.T) {
	path := settingsWith(t, `{"theme":"dark","tui":"fullscreen","skipDangerousModePermissionPrompt":true}`)

	if _, err := Install(path, "/usr/local/bin/ccvault", false); err != nil {
		t.Fatal(err)
	}

	m := readSettings(t, path)
	if m["theme"] != "dark" {
		t.Errorf("theme = %v, want dark", m["theme"])
	}
	if m["tui"] != "fullscreen" {
		t.Errorf("tui = %v, want fullscreen", m["tui"])
	}
	if m["skipDangerousModePermissionPrompt"] != true {
		t.Error("skipDangerousModePermissionPrompt was dropped")
	}
	if _, ok := m["hooks"]; !ok {
		t.Error("hooks block was not added")
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	path := settingsWith(t, `{"theme":"dark"}`)

	first, err := Install(path, "/usr/local/bin/ccvault", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Added) != len(Events) {
		t.Fatalf("first install added %v, want all of %v", first.Added, Events)
	}

	second, err := Install(path, "/usr/local/bin/ccvault", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Added) != 0 {
		t.Errorf("second install added %v, want nothing", second.Added)
	}
	if len(second.AlreadyOK) != len(Events) {
		t.Errorf("second install reported %v already installed, want all", second.AlreadyOK)
	}
}

// Someone else's hook on the same event must not be clobbered.
func TestInstallKeepsForeignHooksOnSameEvent(t *testing.T) {
	path := settingsWith(t, `{
      "hooks": {
        "SessionStart": [
          {"hooks": [{"type": "command", "command": "echo my-own-thing"}]}
        ]
      }
    }`)

	if _, err := Install(path, "/usr/local/bin/ccvault", false); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if !hasCommand(parsed.Hooks["SessionStart"], "echo my-own-thing") {
		t.Error("pre-existing SessionStart hook was removed")
	}
	if !hasCommand(parsed.Hooks["SessionStart"], "/usr/local/bin/ccvault capture --hook") {
		t.Error("ccvault hook was not added alongside the existing one")
	}
}

func TestInstallDryRunWritesNothing(t *testing.T) {
	path := settingsWith(t, `{"theme":"dark"}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := Install(path, "/usr/local/bin/ccvault", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Added) != len(Events) {
		t.Errorf("dry run should report %d additions, got %v", len(Events), plan.Added)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("dry run modified the file")
	}
}

func TestInstallCreatesMissingSettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	if _, err := Install(path, "/usr/local/bin/ccvault", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSettings(t, path)["hooks"]; !ok {
		t.Error("hooks block missing from freshly created settings.json")
	}
}

// Refusing to write is far better than truncating a file Claude Code depends on.
func TestInstallRefusesMalformedSettings(t *testing.T) {
	path := settingsWith(t, `{"theme": "dark",,,`)

	if _, err := Install(path, "/usr/local/bin/ccvault", false); err == nil {
		t.Fatal("expected an error on malformed settings.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"theme": "dark",,,` {
		t.Error("malformed file was modified; it must be left untouched")
	}
}

// SessionStart runs async so it cannot delay startup; SessionEnd must not,
// because an async hook can lose the race with process exit.
func TestHookAsyncPolicy(t *testing.T) {
	if !commandFor("/bin/ccvault", "SessionStart").Async {
		t.Error("SessionStart hook should be async")
	}
	if commandFor("/bin/ccvault", "SessionEnd").Async {
		t.Error("SessionEnd hook must be synchronous to complete before exit")
	}
	for _, e := range Events {
		if commandFor("/bin/ccvault", e).Timeout <= 0 {
			t.Errorf("%s hook has no timeout", e)
		}
	}
}

func TestShellQuoteHandlesSpaces(t *testing.T) {
	got := commandFor("/Applications/My Tools/ccvault", "SessionEnd").Command
	want := `'/Applications/My Tools/ccvault' capture --hook`
	if got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}
