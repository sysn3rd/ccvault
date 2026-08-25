package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// sentinelName marks a directory as a real ccvault vault. Its absence is what
// tells us a configured path is not the vault we think it is — most importantly
// when an external drive is unplugged and its mountpoint is left behind as an
// ordinary empty directory.
const sentinelName = ".ccvault-vault"

type sentinel struct {
	Created time.Time `json:"created"`
	Version int       `json:"version"`
}

func writeSentinel(vaultDir string) error {
	path := filepath.Join(vaultDir, sentinelName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	body, err := json.Marshal(sentinel{Created: time.Now(), Version: 1})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func hasSentinel(vaultDir string) bool {
	_, err := os.Stat(filepath.Join(vaultDir, sentinelName))
	return err == nil
}

// looksLikeVault recognises ccvault's own layout without the sentinel. The
// index is the giveaway: nothing else creates one next to transcripts/ and
// snapshots/.
func looksLikeVault(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "index.db")); err != nil {
		return false
	}
	for _, sub := range []string{"transcripts", "snapshots", "patches"} {
		if info, err := os.Stat(filepath.Join(dir, sub)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// Availability is the state of the configured vault location.
type Availability string

const (
	// VaultOK means the vault is present and usable.
	VaultOK Availability = "ok"
	// VaultUninitialised means nothing has been created yet — first run, or a
	// freshly configured location. Creating it is the obvious next step.
	VaultUninitialised Availability = "uninitialised"
	// VaultUnmounted means the path exists but is not the vault: the filesystem
	// underneath it changed, which is what unplugging a drive looks like.
	// Reconnecting the drive restores it; nothing should be written meanwhile.
	VaultUnmounted Availability = "unmounted"
	// VaultDeleted means the vault was on this same filesystem and is gone.
	// There is nothing to reconnect; the store can only be recreated empty.
	VaultDeleted Availability = "deleted"
	// VaultOccupied means something else is living at the configured path.
	VaultOccupied Availability = "occupied"
)

// Status describes the vault location and how to talk about it.
type Status struct {
	State Availability
	Path  string
	// Detail is a human-readable explanation, safe to print directly.
	Detail string
	// LastSeen is when the vault was last known good, if ever.
	LastSeen time.Time
}

// vaultRecord is local metadata about the vault, kept OFF the vault itself so
// it survives the drive being unplugged. It is what lets us tell "the drive is
// not mounted" apart from "the directory was deleted".
type vaultRecord struct {
	Path     string    `json:"path"`
	Device   uint64    `json:"device"`
	LastSeen time.Time `json:"last_seen"`
}

// StateDir is local bookkeeping — never on the vault, because it has to be
// readable precisely when the vault is not.
func StateDir() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "ccvault"), nil
}

// PendingDir holds captures taken while the vault was unreachable. A hook runs
// inside a live Claude session and cannot stop to ask anything, so it writes
// here rather than losing the commit SHA it just observed. Nothing moves out of
// here without the user saying so.
func PendingDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pending"), nil
}

func recordPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vault.json"), nil
}

func loadRecord() (*vaultRecord, bool) {
	p, err := recordPath()
	if err != nil {
		return nil, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var r vaultRecord
	if json.Unmarshal(raw, &r) != nil {
		return nil, false
	}
	return &r, true
}

// RememberVault records the vault as known-good, including which filesystem it
// is on.
func (c *Config) RememberVault() error {
	dev, err := deviceOf(c.VaultDir)
	if err != nil {
		return err
	}
	p, err := recordPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(vaultRecord{Path: c.VaultDir, Device: dev, LastSeen: time.Now()})
	if err != nil {
		return err
	}
	return os.WriteFile(p, body, 0o600)
}

// Check reports whether the vault is usable, and if not, which kind of not.
func (c *Config) Check() Status {
	s := Status{Path: c.VaultDir}
	rec, haveRec := loadRecord()
	if haveRec && rec.Path == c.VaultDir {
		s.LastSeen = rec.LastSeen
	}

	info, err := os.Stat(c.VaultDir)
	switch {
	case os.IsNotExist(err):
		// The path is gone entirely. If we never knew this vault, it simply has
		// not been created. If we did, whether it can come back depends on
		// whether its filesystem is still here.
		if !haveRec || rec.Path != c.VaultDir {
			s.State = VaultUninitialised
			s.Detail = fmt.Sprintf("%s does not exist yet", c.VaultDir)
			return s
		}
		if mountChanged(c.VaultDir, rec.Device) {
			s.State = VaultUnmounted
			s.Detail = fmt.Sprintf("%s is not available — the drive it lives on is not mounted", c.VaultDir)
			return s
		}
		s.State = VaultDeleted
		s.Detail = fmt.Sprintf("%s was deleted; its filesystem is still mounted, so there is nothing to reconnect", c.VaultDir)
		return s
	case err != nil:
		s.State = VaultOccupied
		s.Detail = fmt.Sprintf("%s cannot be read: %v", c.VaultDir, err)
		return s
	case !info.IsDir():
		s.State = VaultOccupied
		s.Detail = fmt.Sprintf("%s exists but is not a directory", c.VaultDir)
		return s
	}

	if hasSentinel(c.VaultDir) {
		s.State = VaultOK
		return s
	}

	// A vault created before the sentinel existed is still a vault. Recognise
	// it by its own contents and mark it, rather than telling someone their
	// existing data is a stranger's directory.
	if looksLikeVault(c.VaultDir) {
		_ = writeSentinel(c.VaultDir)
		s.State = VaultOK
		return s
	}

	// The directory is there but is not our vault. An unplugged drive leaves
	// its mountpoint behind as an empty directory on the parent filesystem,
	// which is exactly this shape.
	entries, readErr := os.ReadDir(c.VaultDir)
	empty := readErr == nil && len(entries) == 0

	if haveRec && rec.Path == c.VaultDir {
		if dev, err := deviceOf(c.VaultDir); err == nil && dev != rec.Device {
			s.State = VaultUnmounted
			s.Detail = fmt.Sprintf("%s is an empty mountpoint — the drive holding the vault is not mounted", c.VaultDir)
			return s
		}
		if empty {
			s.State = VaultDeleted
			s.Detail = fmt.Sprintf("%s is empty; the vault that was here has been deleted", c.VaultDir)
			return s
		}
	}
	if empty {
		s.State = VaultUninitialised
		s.Detail = fmt.Sprintf("%s is empty and has no vault in it yet", c.VaultDir)
		return s
	}
	s.State = VaultOccupied
	s.Detail = fmt.Sprintf("%s already contains other files and is not a ccvault vault", c.VaultDir)
	return s
}

// mountChanged reports whether the filesystem now showing at a path differs
// from the one the vault was recorded on. When the path itself is gone, the
// nearest existing ancestor is what we can observe.
func mountChanged(path string, recorded uint64) bool {
	dev, err := deviceOf(nearestExisting(path))
	if err != nil {
		return false
	}
	return dev != recorded
}

func nearestExisting(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// Usable is the quick predicate for callers that only care yes/no.
func (c *Config) Usable() bool { return c.Check().State == VaultOK }
