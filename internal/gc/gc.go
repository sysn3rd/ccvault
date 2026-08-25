// Package gc reclaims vault space without ever discarding the irreplaceable.
//
// The ordering principle: a transcript is the one thing that cannot be
// regenerated from anything else, so a referenced transcript is never a
// candidate. Patches, untracked bundles and tree archives are all supporting
// evidence for a restore, and older generations of them are expendable.
package gc

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
)

// GitStatesKept is how many git captures to retain per session. The most recent
// is what a restore uses; a couple more are cheap insurance against the newest
// one having been taken at an unhelpful moment.
const GitStatesKept = 3

type Options struct {
	// OlderThan prunes supporting evidence older than this. Zero disables age
	// pruning, leaving only the per-session retention counts.
	OlderThan time.Duration
}

// Candidate is one thing gc proposes to remove.
type Candidate struct {
	Path   string
	Bytes  int64
	Reason string
	// RowID and Table identify an index row to delete alongside the file.
	RowID int64
	Table string
}

type Plan struct {
	Candidates []Candidate
	FreedBytes int64
	// Dangling are index rows whose file has already vanished; the row is
	// cleaned up but there is nothing to delete.
	Dangling []Candidate
}

type Report struct {
	Removed    int
	FreedBytes int64
	Failed     []string
}

// Compute works out what can go. It never proposes a transcript that a session
// still references.
func Compute(cfg *config.Config, db *index.DB, opts Options) (*Plan, error) {
	p := &Plan{}
	var cutoff time.Time
	if opts.OlderThan > 0 {
		cutoff = time.Now().Add(-opts.OlderThan)
	}

	uuids, err := db.UUIDs()
	if err != nil {
		return nil, err
	}

	// Older git captures, with their patches and untracked bundles.
	for _, uuid := range uuids {
		stale, err := db.StaleGitStates(uuid, GitStatesKept, cutoff)
		if err != nil {
			return nil, err
		}
		for _, g := range stale {
			for _, f := range []string{g.PatchPath, g.UntrackedPath} {
				if f == "" {
					continue
				}
				addFile(p, f, fmt.Sprintf("superseded git capture from %s", g.CapturedAt.Format("2006-01-02")), g.ID, "git_state")
			}
			if g.PatchPath == "" && g.UntrackedPath == "" {
				p.Candidates = append(p.Candidates, Candidate{
					Reason: "superseded git capture (no files)", RowID: g.ID, Table: "git_state",
				})
			}
		}
	}

	// Orphans: anything in the vault no row points at. This is what a `forget`
	// or a manual deletion leaves behind.
	refs, err := db.ReferencedFiles()
	if err != nil {
		return nil, err
	}
	// Files the plan already claims are not orphans as well.
	claimed := map[string]bool{}
	for _, c := range p.Candidates {
		if c.Path != "" {
			claimed[c.Path] = true
		}
	}
	for _, dir := range []string{cfg.TranscriptsDir(), cfg.PatchesDir(), cfg.SnapshotsDir()} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			full := filepath.Join(dir, e.Name())
			if refs[full] || claimed[full] {
				continue
			}
			addFile(p, full, "orphaned (nothing in the index refers to it)", 0, "")
		}
	}

	sort.Slice(p.Candidates, func(i, j int) bool { return p.Candidates[i].Path < p.Candidates[j].Path })
	return p, nil
}

func addFile(p *Plan, path, reason string, rowID int64, table string) {
	info, err := os.Stat(path)
	if err != nil {
		// Already gone: still worth clearing the row that points at it.
		p.Dangling = append(p.Dangling, Candidate{Path: path, Reason: "file already missing", RowID: rowID, Table: table})
		return
	}
	p.Candidates = append(p.Candidates, Candidate{
		Path: path, Bytes: info.Size(), Reason: reason, RowID: rowID, Table: table,
	})
	p.FreedBytes += info.Size()
}

// Execute performs the plan.
func Execute(db *index.DB, p *Plan) (*Report, error) {
	r := &Report{}
	seenRows := map[string]bool{}

	for _, c := range append(append([]Candidate{}, p.Candidates...), p.Dangling...) {
		if c.Path != "" {
			if err := os.Remove(c.Path); err != nil && !os.IsNotExist(err) {
				r.Failed = append(r.Failed, c.Path+": "+err.Error())
				continue
			}
			r.Removed++
			r.FreedBytes += c.Bytes
		}
		if c.Table == "" || c.RowID == 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", c.Table, c.RowID)
		if seenRows[key] {
			continue // one row can own two files
		}
		seenRows[key] = true
		switch c.Table {
		case "git_state":
			if err := db.DeleteGitState(c.RowID); err != nil {
				r.Failed = append(r.Failed, err.Error())
			}
		case "snapshots":
			if err := db.DeleteSnapshot(c.RowID); err != nil {
				r.Failed = append(r.Failed, err.Error())
			}
		}
	}
	return r, nil
}
