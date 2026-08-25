// Package config resolves the filesystem locations ccvault reads from and writes to.
//
// Everything is overridable by environment variable so tests can run against a
// throwaway tree without touching the user's real vault or Claude state.
package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	// ClaudeHome is ~/.claude — the directory Claude Code owns. Read-only to us,
	// with the single exception of settings.json during install-hooks.
	ClaudeHome string
	// VaultDir holds our own copies. Created 0700.
	VaultDir string
}

func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	claudeHome := os.Getenv("CCVAULT_CLAUDE_HOME")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}

	vault := os.Getenv("CCVAULT_HOME")
	if vault == "" {
		// XDG on Linux; the same layout on macOS deliberately, so the two
		// machines stay symmetric and a synced vault would drop in unchanged.
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(home, ".local", "share")
		}
		vault = filepath.Join(data, "ccvault")
	}

	return &Config{ClaudeHome: claudeHome, VaultDir: vault}, nil
}

// ProjectsDir is where Claude Code shards transcripts by slugified cwd.
// The slug is lossy (/, . and _ all become -) so it is only ever used to
// locate files, never to recover a path. See VISION.md.
func (c *Config) ProjectsDir() string { return filepath.Join(c.ClaudeHome, "projects") }

// HistoryFile is Claude Code's global prompt log: one JSON object per prompt
// carrying display text, timestamp, project path and session id.
func (c *Config) HistoryFile() string { return filepath.Join(c.ClaudeHome, "history.jsonl") }

func (c *Config) SettingsFile() string { return filepath.Join(c.ClaudeHome, "settings.json") }

func (c *Config) DBPath() string         { return filepath.Join(c.VaultDir, "index.db") }
func (c *Config) TranscriptsDir() string { return filepath.Join(c.VaultDir, "transcripts") }
func (c *Config) PatchesDir() string     { return filepath.Join(c.VaultDir, "patches") }
func (c *Config) SnapshotsDir() string   { return filepath.Join(c.VaultDir, "snapshots") }

// EnsureDirs creates the vault tree. Transcripts contain full file contents and
// command output, so the tree is owner-only.
func (c *Config) EnsureDirs() error {
	for _, d := range []string{c.VaultDir, c.TranscriptsDir(), c.PatchesDir(), c.SnapshotsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
