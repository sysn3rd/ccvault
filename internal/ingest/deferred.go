package ingest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/gitstate"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/pending"
	"github.com/sysn3rd/ccvault/internal/snapshot"
	"github.com/sysn3rd/ccvault/internal/transcript"
	"github.com/sysn3rd/ccvault/internal/vault"
)

// CaptureDeferred records a capture on local disk when the vault is unreachable.
//
// The alternative — skipping — throws away the commit SHA and the uncommitted
// diff, which no later scan can recover. Writing them here costs a few hundred
// kilobytes and keeps the decision about where they belong with the user.
func CaptureDeferred(cfg *config.Config, transcriptPath, event string) error {
	dir, err := config.PendingDir()
	if err != nil {
		return err
	}
	store, err := pending.Open(dir)
	if err != nil {
		return err
	}

	ts, err := transcript.Parse(transcriptPath)
	if err != nil || ts.UUID == "" {
		return err
	}

	now := time.Now()
	stamp := now.UnixNano()
	rec := &pending.Record{
		SessionUUID: ts.UUID,
		CapturedAt:  now,
		Event:       event,
		CWD:         ts.CWD,
	}

	// The transcript is re-copied each time rather than diffed; it is the one
	// artefact that must be complete.
	if name, err := store.StageFile(transcriptPath, ts.UUID+".jsonl"); err == nil {
		rec.TranscriptFile = name
	}

	if dirExists(ts.CWD) {
		gs, _ := gitstate.Capture(ts.CWD)
		if gs != nil && gs.IsRepo {
			untracked, _ := json.Marshal(gs.Untracked)
			submodules, _ := json.Marshal(gs.Submodules)
			g := &pending.GitCapture{
				WorktreeRoot: gs.WorktreeRoot, RemoteURL: gs.RemoteURL,
				Branch: gs.Branch, HeadSHA: gs.HeadSHA, IsDirty: gs.IsDirty,
				UntrackedJSON: string(untracked), SubmodulesJSON: string(submodules),
				ClaudeDirTracked: gs.ClaudeDirTracked,
			}
			if gs.Patch != "" {
				patch := gs.Patch
				if patch[len(patch)-1] != '\n' {
					patch += "\n"
				}
				g.PatchFile, _ = store.WriteBytes(fmt.Sprintf("%s-%d.patch", ts.UUID, stamp), []byte(patch))
			}
			if len(gs.Untracked) > 0 {
				bundleName := fmt.Sprintf("%s-%d.untracked.tar.gz", ts.UUID, stamp)
				tmp, err := vault.WriteUntracked(store.Dir(), ts.UUID, gs.WorktreeRoot, now, gs.Untracked)
				if err == nil && tmp.Path != "" {
					_ = os.Rename(tmp.Path, filepath.Join(store.Dir(), bundleName))
					g.UntrackedFile = bundleName
					if len(tmp.Skipped) > 0 {
						b, _ := json.Marshal(tmp.Skipped)
						g.UntrackedSkipped = string(b)
					}
				}
			}
			rec.Git = g
		}

		kind, opts, root, want := snapshotPlan(cfg, gs, ts.CWD)
		if want && dirExists(root) {
			if m, err := snapshot.Plan(root, opts); err == nil && len(m.Files) > 0 {
				name := fmt.Sprintf("%s-%d.%s.tar.gz", ts.UUID, stamp, kind)
				if _, err := snapshot.Write(m, filepath.Join(store.Dir(), name)); err == nil {
					rec.SnapshotFile = name
					rec.SnapshotKind = kind
					rec.TreeHash = m.TreeHash
					rec.FileCount = len(m.Files)
					rec.IncludeGit = opts.IncludeGit
					if len(m.Skipped) > 0 {
						b, _ := json.Marshal(m.Skipped)
						rec.SkippedJSON = string(b)
					}
				}
			}
		}
	}

	return store.Save(rec)
}

// AdoptResult reports what moving pending captures into the vault achieved.
type AdoptResult struct {
	Adopted int
	Failed  []string
}

// Adopt moves everything waiting on local disk into the vault. It is only ever
// called because a person asked for it.
func Adopt(cfg *config.Config, db *index.DB) (*AdoptResult, error) {
	dir, err := config.PendingDir()
	if err != nil {
		return nil, err
	}
	store, err := pending.Open(dir)
	if err != nil {
		return nil, err
	}
	records, err := store.List()
	if err != nil {
		return nil, err
	}

	res := &AdoptResult{}
	for _, r := range records {
		if err := adoptOne(cfg, db, store, r); err != nil {
			res.Failed = append(res.Failed, r.SessionUUID[:8]+": "+err.Error())
			continue
		}
		_ = store.Remove(r)
		res.Adopted++
	}
	return res, nil
}

func adoptOne(cfg *config.Config, db *index.DB, store *pending.Store, r *pending.Record) error {
	// The session row is rebuilt from the transcript we held, so the adopted
	// session is indexed exactly as a live capture would have indexed it.
	if r.TranscriptFile != "" {
		src := filepath.Join(store.Dir(), r.TranscriptFile)
		if ts, err := transcript.Parse(src); err == nil && ts.UUID != "" {
			s := &index.Session{
				UUID: ts.UUID, CWD: ts.CWD, CWDs: ts.CWDs, Title: ts.Title,
				FirstPrompt: ts.FirstPrompt, StartedAt: ts.StartedAt,
				LastActive: ts.LastActive, MsgCount: ts.MsgCount,
				CCVersion: ts.Version, GitBranch: ts.GitBranch,
				Bytes: ts.Bytes, SHA256: ts.SHA256,
				Kind: index.KindPlain, DirState: index.DirMissing,
			}
			if r.Git != nil && r.Git.WorktreeRoot != "" {
				s.Kind = index.KindRepo
			}
			if dirExists(ts.CWD) {
				s.DirState = index.DirOK
				if empty, err := isEmptyDir(ts.CWD); err == nil && empty {
					s.DirState = index.DirEmpty
				}
			}
			dst, err := vault.CopyTranscript(src, cfg.TranscriptsDir(), ts.UUID)
			if err != nil {
				return err
			}
			s.VaultPath = dst
			if err := db.UpsertSession(s, ts.Body); err != nil {
				return err
			}
		}
	}

	if g := r.Git; g != nil {
		patchPath, err := moveInto(store.Dir(), g.PatchFile, cfg.PatchesDir())
		if err != nil {
			return err
		}
		untrackedPath, err := moveInto(store.Dir(), g.UntrackedFile, cfg.PatchesDir())
		if err != nil {
			return err
		}
		if err := db.InsertGitState(&index.GitState{
			SessionUUID: r.SessionUUID, CapturedAt: r.CapturedAt, Event: r.Event,
			WorktreeRoot: g.WorktreeRoot, RemoteURL: g.RemoteURL, Branch: g.Branch,
			HeadSHA: g.HeadSHA, IsDirty: g.IsDirty, PatchPath: patchPath,
			UntrackedJSON: g.UntrackedJSON, SubmodulesJSON: g.SubmodulesJSON,
			ClaudeDirTracked: g.ClaudeDirTracked, UntrackedPath: untrackedPath,
			UntrackedSkipped: g.UntrackedSkipped,
		}); err != nil {
			return err
		}
	}

	if r.SnapshotFile != "" && r.TreeHash != "" {
		// Content-addressed on arrival, same as a live capture, so a deferred
		// snapshot of an unchanged directory does not become a second copy.
		dst := filepath.Join(cfg.SnapshotsDir(), r.TreeHash[:16]+".tar.gz")
		src := filepath.Join(store.Dir(), r.SnapshotFile)
		size, err := adoptSnapshotFile(src, dst)
		if err != nil {
			return err
		}
		if err := db.InsertSnapshot(&index.Snapshot{
			SessionUUID: r.SessionUUID, CapturedAt: r.CapturedAt, Kind: r.SnapshotKind,
			Path: dst, Bytes: size, TreeHash: r.TreeHash, FileCount: r.FileCount,
			IncludeGit: r.IncludeGit, SkippedJSON: r.SkippedJSON,
		}); err != nil {
			return err
		}
		pruneSnapshots(db, r.SessionUUID, r.SnapshotKind, cfg.Retention.Snapshots)
	}
	return nil
}

func adoptSnapshotFile(src, dst string) (int64, error) {
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return st.Size(), nil // identical content already in the vault
	}
	if err := moveFile(src, dst); err != nil {
		return 0, err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func moveInto(srcDir, name, dstDir string) (string, error) {
	if name == "" {
		return "", nil
	}
	dst := filepath.Join(dstDir, name)
	if err := moveFile(filepath.Join(srcDir, name), dst); err != nil {
		return "", err
	}
	return dst, nil
}

// moveFile renames where it can and copies across filesystems, which is the
// normal case here: the holding area is local and the vault may not be.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}
