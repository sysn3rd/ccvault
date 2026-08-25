// Package config resolves where ccvault reads from and writes to.
//
// The settings live in a file rather than the environment for one specific
// reason: ccvault runs from three different places — the CLI you type, a hook
// spawned inside a Claude session, and a systemd timer or launchd agent — and
// those three do not share an environment. An exported variable reaches the
// hook (it inherits your shell) but not the timer (it has none), which quietly
// splits the vault in two. A file is seen identically by all three.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// File is the on-disk shape of the settings. Every field is optional; anything
// unset falls back to a documented default.
type File struct {
	VaultDir   string `toml:"vault_dir"`
	ClaudeHome string `toml:"claude_home"`

	Snapshots SnapshotSettings  `toml:"snapshots"`
	Retention RetentionSettings `toml:"retention"`
	Picker    PickerSettings    `toml:"picker"`
}

type SnapshotSettings struct {
	MaxFileMB  int      `toml:"max_file_mb"`
	MaxTotalMB int      `toml:"max_total_mb"`
	IgnoreDirs []string `toml:"ignore_dirs"`
}

type RetentionSettings struct {
	Snapshots int `toml:"snapshots_kept"`
	GitStates int `toml:"git_states_kept"`
}

type PickerSettings struct {
	// Terminal is the command used to open a new terminal window for a resumed
	// session. Empty means auto-detect per platform.
	Terminal string `toml:"terminal"`
}

type Config struct {
	// ClaudeHome is ~/.claude — Claude Code's own directory. Read-only to us,
	// with the single exception of settings.json during install-hooks.
	ClaudeHome string
	// VaultDir holds our copies. Created 0700.
	VaultDir string

	Snapshots SnapshotSettings
	Retention RetentionSettings
	Picker    PickerSettings

	// Path is the settings file this came from, whether or not it exists yet.
	Path string
	// Loaded reports whether a settings file was actually read.
	Loaded bool
	// FromEnv records settings overridden by the environment, which is
	// supported for tests but is not how a person should relocate the vault.
	FromEnv []string
}

// Defaults, applied to anything the settings file leaves unset.
const (
	DefaultMaxFileMB     = 16
	DefaultMaxTotalMB    = 256
	DefaultSnapshotsKept = 3
	DefaultGitStatesKept = 3
)

// Path returns where the settings file lives.
func Path() (string, error) {
	if p := os.Getenv("CCVAULT_CONFIG"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "ccvault", "config.toml"), nil
}

// DefaultVaultDir is where the vault lives when the settings say nothing.
// The same layout is used on macOS deliberately, so a vault carried between
// the two machines drops in unchanged.
func DefaultVaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "ccvault"), nil
}

// Load reads the settings file and applies defaults.
func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path, err := Path()
	if err != nil {
		return nil, err
	}

	c := &Config{Path: path}
	var f File
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		if err := toml.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("%s is not valid TOML: %w", path, err)
		}
		c.Loaded = true
	case os.IsNotExist(err):
		// No settings file: defaults all the way down.
	default:
		return nil, err
	}

	c.ClaudeHome = expand(f.ClaudeHome, home)
	if c.ClaudeHome == "" {
		c.ClaudeHome = filepath.Join(home, ".claude")
	}
	c.VaultDir = expand(f.VaultDir, home)
	if c.VaultDir == "" {
		if c.VaultDir, err = DefaultVaultDir(); err != nil {
			return nil, err
		}
	}

	c.Snapshots = f.Snapshots
	if c.Snapshots.MaxFileMB <= 0 {
		c.Snapshots.MaxFileMB = DefaultMaxFileMB
	}
	if c.Snapshots.MaxTotalMB <= 0 {
		c.Snapshots.MaxTotalMB = DefaultMaxTotalMB
	}
	// A nil list means "use the snapshot package's defaults"; an empty list
	// written explicitly means "ignore nothing", and both are honoured.
	c.Retention = f.Retention
	if c.Retention.Snapshots <= 0 {
		c.Retention.Snapshots = DefaultSnapshotsKept
	}
	if c.Retention.GitStates <= 0 {
		c.Retention.GitStates = DefaultGitStatesKept
	}
	c.Picker = f.Picker

	// Environment overrides exist for tests and one-off inspection. They are
	// deliberately reported, because a vault that moves per-process is the
	// failure this package is designed to prevent.
	if v := os.Getenv("CCVAULT_HOME"); v != "" {
		c.VaultDir = v
		c.FromEnv = append(c.FromEnv, "CCVAULT_HOME")
	}
	if v := os.Getenv("CCVAULT_CLAUDE_HOME"); v != "" {
		c.ClaudeHome = v
		c.FromEnv = append(c.FromEnv, "CCVAULT_CLAUDE_HOME")
	}
	return c, nil
}

// Save writes the settings file, creating its directory.
func (c *Config) Save() error {
	f := File{
		VaultDir:   c.VaultDir,
		ClaudeHome: c.ClaudeHome,
		Snapshots:  c.Snapshots,
		Retention:  c.Retention,
		Picker:     c.Picker,
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.Path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(header); err != nil {
		tmp.Close()
		return err
	}
	if err := toml.NewEncoder(tmp).Encode(f); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.Path)
}

const header = `# ccvault settings.
#
# Read by everything: the CLI, the capture hooks inside Claude sessions, and the
# periodic reconcile job. That is why the vault location belongs here rather
# than in an environment variable — those three do not share an environment,
# and a variable that only some of them see splits the vault in two.
#
# Edit by hand, or use: ccvault config set vault_dir /path, or the settings
# screen in ccvault search (press ,).

`

func expand(p, home string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func (c *Config) ProjectsDir() string    { return filepath.Join(c.ClaudeHome, "projects") }
func (c *Config) HistoryFile() string    { return filepath.Join(c.ClaudeHome, "history.jsonl") }
func (c *Config) SettingsFile() string   { return filepath.Join(c.ClaudeHome, "settings.json") }
func (c *Config) DBPath() string         { return filepath.Join(c.VaultDir, "index.db") }
func (c *Config) TranscriptsDir() string { return filepath.Join(c.VaultDir, "transcripts") }
func (c *Config) PatchesDir() string     { return filepath.Join(c.VaultDir, "patches") }
func (c *Config) SnapshotsDir() string   { return filepath.Join(c.VaultDir, "snapshots") }

// EnsureDirs creates the vault tree. Transcripts hold full file contents and
// command output, so the tree is owner-only.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.VaultDir, c.TranscriptsDir(), c.PatchesDir(), c.SnapshotsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return writeSentinel(c.VaultDir)
}
