package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/gitstate"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/snapshot"
)

// snapshotPlan decides what a session needs archived, given its git state.
//
// The three cases, in order of how much git already covers:
//   - a repository with a reachable remote needs no tree archive at all; remote
//     plus SHA plus patch reconstructs it. Only an untracked .claude/ is at risk,
//     because cloning silently returns without the project's skills and agents.
//   - a repository with no remote has nowhere to be cloned from, so the archive
//     must include .git or the history is lost.
//   - a plain directory has no git safety net whatsoever.
func snapshotPlan(cfg *config.Config, gs *gitstate.State, cwd string) (kind string, opts snapshot.Options, root string, want bool) {
	base := snapshotOptions(cfg)
	if gs == nil || !gs.IsRepo {
		return index.SnapTree, base, cwd, true
	}
	if gs.RemoteURL == "" {
		withGit := base
		withGit.IncludeGit = true
		return index.SnapTree, withGit, gs.WorktreeRoot, true
	}
	if !gs.ClaudeDirTracked {
		claudeDir := filepath.Join(gs.WorktreeRoot, ".claude")
		if dirExists(claudeDir) {
			return index.SnapClaude, base, claudeDir, true
		}
	}
	return "", snapshot.Options{}, "", false
}

// snapshotOptions translates the settings file into archiving limits. A nil
// ignore list is left nil so the snapshot package applies its own defaults.
func snapshotOptions(cfg *config.Config) snapshot.Options {
	return snapshot.Options{
		IgnoreDirs:    cfg.Snapshots.IgnoreDirs,
		MaxFileBytes:  int64(cfg.Snapshots.MaxFileMB) << 20,
		MaxTotalBytes: int64(cfg.Snapshots.MaxTotalMB) << 20,
	}
}

// captureSnapshot archives the session's directory when the plan calls for it.
//
// An unchanged directory costs one walk: the tree hash is compared against the
// most recent archive and matching content is not rewritten.
func captureSnapshot(cfg *config.Config, db *index.DB, uuid string, gs *gitstate.State, cwd string) error {
	kind, opts, root, want := snapshotPlan(cfg, gs, cwd)
	if !want || !dirExists(root) {
		return nil
	}

	m, err := snapshot.Plan(root, opts)
	if err != nil {
		return err
	}
	if len(m.Files) == 0 {
		return nil // nothing to archive; an empty directory needs no artefact
	}

	if latest, err := db.LatestSnapshot(uuid, kind); err == nil && latest != nil {
		if latest.TreeHash == m.TreeHash {
			if _, statErr := os.Stat(latest.Path); statErr == nil {
				return nil // unchanged since last time
			}
		}
	}

	now := time.Now()
	// Content-addressed: many sessions share one working directory (every
	// session in ~/Work, for instance), and naming the archive by its tree hash
	// means they share one file instead of storing N identical copies.
	dest := filepath.Join(cfg.SnapshotsDir(), m.TreeHash[:16]+".tar.gz")
	size, err := reuseOrWrite(m, dest)
	if err != nil {
		return err
	}
	if size == 0 {
		return nil
	}

	var skipped string
	if len(m.Skipped) > 0 {
		b, _ := json.Marshal(m.Skipped)
		skipped = string(b)
	}
	if err := db.InsertSnapshot(&index.Snapshot{
		SessionUUID: uuid,
		CapturedAt:  now,
		Kind:        kind,
		Path:        dest,
		Bytes:       size,
		TreeHash:    m.TreeHash,
		FileCount:   len(m.Files),
		IncludeGit:  opts.IncludeGit,
		SkippedJSON: skipped,
	}); err != nil {
		return err
	}

	pruneSnapshots(db, uuid, kind, cfg.Retention.Snapshots)
	return nil
}

// reuseOrWrite skips the archive entirely when one with this exact content
// already exists.
func reuseOrWrite(m *snapshot.Manifest, dest string) (int64, error) {
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return st.Size(), nil
	}
	return snapshot.Write(m, dest)
}

func pruneSnapshots(db *index.DB, uuid, kind string, keep int) {
	stale, err := db.StaleSnapshots(uuid, kind, keep)
	if err != nil {
		return
	}
	for _, s := range stale {
		if err := db.DeleteSnapshot(s.ID); err != nil {
			continue
		}
		// The file is shared, so it only goes when nothing else points at it.
		if refs, err := db.CountSnapshotRefs(s.Path); err == nil && refs == 0 {
			_ = os.Remove(s.Path)
		}
	}
}
