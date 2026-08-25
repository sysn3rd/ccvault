// Package pending holds captures taken while the vault was unreachable.
//
// A capture hook runs inside a live Claude session and must never stop to ask a
// question, so it cannot prompt for a new vault location when an external drive
// is unplugged. Skipping instead would permanently lose the one thing that
// cannot be recovered later — the commit the session was sitting on. So the
// hook writes here, on local disk, and nothing leaves this directory until the
// user says where it should go.
package pending

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Record is one deferred capture. Files it refers to are siblings of the record
// itself, named relative to the pending directory.
type Record struct {
	SessionUUID string    `json:"session_uuid"`
	CapturedAt  time.Time `json:"captured_at"`
	Event       string    `json:"event"`
	CWD         string    `json:"cwd"`

	TranscriptFile string `json:"transcript_file,omitempty"`

	Git *GitCapture `json:"git,omitempty"`

	SnapshotFile string `json:"snapshot_file,omitempty"`
	SnapshotKind string `json:"snapshot_kind,omitempty"`
	TreeHash     string `json:"tree_hash,omitempty"`
	FileCount    int    `json:"file_count,omitempty"`
	IncludeGit   bool   `json:"include_git,omitempty"`
	SkippedJSON  string `json:"skipped_json,omitempty"`

	// path is where this record was read from; not serialised.
	path string
}

type GitCapture struct {
	WorktreeRoot     string `json:"worktree_root,omitempty"`
	RemoteURL        string `json:"remote_url,omitempty"`
	Branch           string `json:"branch,omitempty"`
	HeadSHA          string `json:"head_sha,omitempty"`
	IsDirty          bool   `json:"is_dirty,omitempty"`
	PatchFile        string `json:"patch_file,omitempty"`
	UntrackedFile    string `json:"untracked_file,omitempty"`
	UntrackedJSON    string `json:"untracked_json,omitempty"`
	SubmodulesJSON   string `json:"submodules_json,omitempty"`
	ClaudeDirTracked bool   `json:"claude_dir_tracked,omitempty"`
	UntrackedSkipped string `json:"untracked_skipped,omitempty"`
}

// Path returns where a record lives on disk.
func (r *Record) Path() string { return r.path }

// Dir returns the directory holding a record's files.
func (r *Record) Dir() string { return filepath.Dir(r.path) }

// Store writes captures into a directory, which is created 0700 — the files
// here carry the same secrets the vault does.
type Store struct{ dir string }

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

// StageFile copies a file into the pending area under a stable name and returns
// that name, for the record to refer to.
func (s *Store) StageFile(src, name string) (string, error) {
	if src == "" {
		return "", nil
	}
	dst := filepath.Join(s.dir, name)
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", err
	}
	return name, nil
}

// WriteBytes stores raw content (a patch, say) as a pending file.
func (s *Store) WriteBytes(name string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	if err := os.WriteFile(filepath.Join(s.dir, name), data, 0o600); err != nil {
		return "", err
	}
	return name, nil
}

// Save writes the record itself. It is written last, so a record on disk always
// means its files are already there.
func (s *Store) Save(r *Record) error {
	name := fmt.Sprintf("%s-%d.json", r.SessionUUID, r.CapturedAt.UnixNano())
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, name), body, 0o600)
}

// List returns every pending capture, oldest first.
func (s *Store) List() ([]*Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		full := filepath.Join(s.dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil {
			continue // a malformed record is not worth failing the listing
		}
		r.path = full
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CapturedAt.Before(out[j].CapturedAt) })
	return out, nil
}

// Count is how many captures are waiting.
func (s *Store) Count() int {
	rs, err := s.List()
	if err != nil {
		return 0
	}
	return len(rs)
}

// Sessions is the set of distinct sessions represented, which is what a person
// actually wants to be told.
func (s *Store) Sessions() int {
	rs, err := s.List()
	if err != nil {
		return 0
	}
	seen := map[string]bool{}
	for _, r := range rs {
		seen[r.SessionUUID] = true
	}
	return len(seen)
}

// Bytes is the total size held.
func (s *Store) Bytes() int64 {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// Remove deletes a record and the files it owns, once it has been adopted.
func (s *Store) Remove(r *Record) error {
	names := []string{r.TranscriptFile, r.SnapshotFile}
	if r.Git != nil {
		names = append(names, r.Git.PatchFile, r.Git.UntrackedFile)
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, n))
	}
	return os.Remove(r.path)
}
