package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolate points every location at a throwaway tree.
func isolate(t *testing.T) (vault string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CCVAULT_HOME", "")
	t.Setenv("CCVAULT_CLAUDE_HOME", "")
	t.Setenv("CCVAULT_CONFIG", "")
	return filepath.Join(root, "data", "ccvault")
}

func TestDefaultsWithNoSettingsFile(t *testing.T) {
	want := isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Loaded {
		t.Error("Loaded = true with no settings file")
	}
	if c.VaultDir != want {
		t.Errorf("VaultDir = %q, want %q", c.VaultDir, want)
	}
	if c.Snapshots.MaxFileMB != DefaultMaxFileMB || c.Retention.GitStates != DefaultGitStatesKept {
		t.Errorf("defaults not applied: %+v %+v", c.Snapshots, c.Retention)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.VaultDir = "/mnt/backup/ccvault"
	c.Snapshots.MaxFileMB = 64
	c.Snapshots.IgnoreDirs = []string{"node_modules", "coverage"}
	c.Retention.GitStates = 7
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !again.Loaded {
		t.Error("Loaded = false after Save")
	}
	if again.VaultDir != "/mnt/backup/ccvault" {
		t.Errorf("VaultDir = %q", again.VaultDir)
	}
	if again.Snapshots.MaxFileMB != 64 {
		t.Errorf("MaxFileMB = %d, want 64", again.Snapshots.MaxFileMB)
	}
	if strings.Join(again.Snapshots.IgnoreDirs, ",") != "node_modules,coverage" {
		t.Errorf("IgnoreDirs = %v", again.Snapshots.IgnoreDirs)
	}
	if again.Retention.GitStates != 7 {
		t.Errorf("GitStates = %d, want 7", again.Retention.GitStates)
	}

	// The file should be readable by a person, and say why it exists.
	raw, err := os.ReadFile(again.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "vault_dir") {
		t.Error("settings file does not contain vault_dir")
	}
	if !strings.Contains(string(raw), "environment") {
		t.Error("settings file should explain why settings live here rather than in the environment")
	}
}

func TestTildeIsExpanded(t *testing.T) {
	isolate(t)
	home, _ := os.UserHomeDir()
	c, _ := Load()
	c.VaultDir = "~/backups/ccvault"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.VaultDir != filepath.Join(home, "backups", "ccvault") {
		t.Errorf("VaultDir = %q, want the expanded home path", again.VaultDir)
	}
}

// An environment override reaches the hook but not the timer, so it must be
// reported rather than applied silently.
func TestEnvOverrideIsReported(t *testing.T) {
	isolate(t)
	t.Setenv("CCVAULT_HOME", "/tmp/somewhere-else")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.VaultDir != "/tmp/somewhere-else" {
		t.Errorf("VaultDir = %q", c.VaultDir)
	}
	if len(c.FromEnv) != 1 || c.FromEnv[0] != "CCVAULT_HOME" {
		t.Errorf("FromEnv = %v, want [CCVAULT_HOME]", c.FromEnv)
	}
}

func TestCheckUninitialisedThenOK(t *testing.T) {
	isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if st := c.Check(); st.State != VaultUninitialised {
		t.Fatalf("State = %q, want uninitialised", st.State)
	}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := c.RememberVault(); err != nil {
		t.Fatal(err)
	}
	if st := c.Check(); st.State != VaultOK {
		t.Errorf("State = %q after creation, want ok (%s)", st.State, st.Detail)
	}
}

// The vault deleted from a filesystem that is still mounted has nothing to
// reconnect — recreating it empty is the only option, and saying so matters.
func TestCheckDetectsDeletedVault(t *testing.T) {
	isolate(t)
	c, _ := Load()
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := c.RememberVault(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.VaultDir); err != nil {
		t.Fatal(err)
	}

	st := c.Check()
	if st.State != VaultDeleted {
		t.Fatalf("State = %q, want deleted (%s)", st.State, st.Detail)
	}
	if !strings.Contains(st.Detail, "nothing to reconnect") {
		t.Errorf("Detail should make clear reconnecting will not help: %q", st.Detail)
	}
}

// An unplugged drive leaves its mountpoint as an empty directory on a different
// filesystem. That must read as "reconnect", never as "recreate empty".
func TestCheckDetectsUnmountedDrive(t *testing.T) {
	isolate(t)
	c, _ := Load()
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := c.RememberVault(); err != nil {
		t.Fatal(err)
	}

	// Empty the directory and claim it was recorded on a different device,
	// which is precisely the state an unmounted volume leaves behind.
	if err := os.RemoveAll(c.VaultDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	forceRecordedDevice(t, c.VaultDir, 999999)

	st := c.Check()
	if st.State != VaultUnmounted {
		t.Fatalf("State = %q, want unmounted (%s)", st.State, st.Detail)
	}
	if !strings.Contains(st.Detail, "not mounted") {
		t.Errorf("Detail should say the drive is not mounted: %q", st.Detail)
	}
}

// A vault created before the sentinel existed is still a vault; refusing to
// recognise it would lock someone out of their own data.
func TestCheckAdoptsPreSentinelVault(t *testing.T) {
	isolate(t)
	c, _ := Load()
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.VaultDir, "index.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.VaultDir, sentinelName)); err != nil {
		t.Fatal(err)
	}

	if st := c.Check(); st.State != VaultOK {
		t.Fatalf("State = %q, want ok for a pre-sentinel vault (%s)", st.State, st.Detail)
	}
	if !hasSentinel(c.VaultDir) {
		t.Error("recognising a pre-sentinel vault should also mark it")
	}
}

// Somebody else's directory must not be adopted or written into.
func TestCheckRefusesOccupiedDirectory(t *testing.T) {
	isolate(t)
	c, _ := Load()
	if err := os.MkdirAll(c.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.VaultDir, "someone-elses-notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := c.Check(); st.State != VaultOccupied {
		t.Errorf("State = %q, want occupied (%s)", st.State, st.Detail)
	}
}

func forceRecordedDevice(t *testing.T, vaultPath string, dev uint64) {
	t.Helper()
	p, err := recordPath()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(vaultRecord{Path: vaultPath, Device: dev, LastSeen: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
