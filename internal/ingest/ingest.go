// Package ingest turns Claude Code's on-disk session state into vault records.
//
// Two entry points feed the same pipeline: hooks fire at the exact moment a
// session starts and ends (the only time accurate git provenance is available),
// and a periodic scan reconciles everything hooks miss — crashes, kill -9, and
// every session that predates installation.
package ingest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/gitstate"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/transcript"
	"github.com/sysn3rd/ccvault/internal/vault"
)

type Result struct {
	Scanned int
	Updated int
	Skipped int
	Failed  int
}

// Ingest processes one transcript file. event is "start", "end" or "scan" and is
// recorded alongside the git capture so restore can prefer the most informative one.
func Ingest(cfg *config.Config, db *index.DB, transcriptPath, event string, captureGit bool) (*index.Session, error) {
	ts, err := transcript.Parse(transcriptPath)
	if err != nil {
		return nil, err
	}
	if ts.UUID == "" {
		return nil, nil // no session id anywhere in the file; nothing to key on
	}

	s := &index.Session{
		UUID:        ts.UUID,
		CWD:         ts.CWD,
		CWDs:        ts.CWDs,
		Slug:        filepath.Base(filepath.Dir(transcriptPath)),
		Title:       ts.Title,
		FirstPrompt: ts.FirstPrompt,
		StartedAt:   ts.StartedAt,
		LastActive:  ts.LastActive,
		MsgCount:    ts.MsgCount,
		CCVersion:   ts.Version,
		GitBranch:   ts.GitBranch,
		Bytes:       ts.Bytes,
		SHA256:      ts.SHA256,
		Kind:        index.KindPlain,
		DirState:    index.DirMissing,
	}

	var gs *gitstate.State
	if dirExists(ts.CWD) {
		s.DirState = index.DirOK
		if captureGit {
			gs, _ = gitstate.Capture(ts.CWD)
		} else {
			// Cheap classification without shelling out to git.
			gs = &gitstate.State{IsRepo: exists(filepath.Join(ts.CWD, ".git"))}
		}
		if gs != nil && gs.IsRepo {
			s.Kind = index.KindRepo
		}
	}

	vp, err := vault.CopyTranscript(transcriptPath, cfg.TranscriptsDir(), ts.UUID)
	if err != nil {
		return nil, err
	}
	s.VaultPath = vp

	if err := db.UpsertSession(s, ts.Body); err != nil {
		return nil, err
	}

	if captureGit && gs != nil && gs.IsRepo {
		now := time.Now()
		patchPath, _ := vault.WritePatch(cfg.PatchesDir(), ts.UUID, now, gs.Patch)
		untracked, _ := json.Marshal(gs.Untracked)
		submodules, _ := json.Marshal(gs.Submodules)

		// `git diff HEAD` covers tracked files only; untracked file contents
		// vanish with the directory unless they are archived here and now.
		bundle, _ := vault.WriteUntracked(cfg.PatchesDir(), ts.UUID, gs.WorktreeRoot, now, gs.Untracked)
		var skipped string
		if bundle != nil && len(bundle.Skipped) > 0 {
			b, _ := json.Marshal(bundle.Skipped)
			skipped = string(b)
		}
		bundlePath := ""
		if bundle != nil {
			bundlePath = bundle.Path
		}
		_ = db.InsertGitState(&index.GitState{
			SessionUUID:      ts.UUID,
			CapturedAt:       now,
			Event:            event,
			WorktreeRoot:     gs.WorktreeRoot,
			RemoteURL:        gs.RemoteURL,
			Branch:           gs.Branch,
			HeadSHA:          gs.HeadSHA,
			IsDirty:          gs.IsDirty,
			PatchPath:        patchPath,
			UntrackedJSON:    string(untracked),
			SubmodulesJSON:   string(submodules),
			ClaudeDirTracked: gs.ClaudeDirTracked,
			UntrackedPath:    bundlePath,
			UntrackedSkipped: skipped,
		})
	}

	return s, nil
}

// ScanAll walks every project shard and ingests anything new or grown.
//
// Transcripts are append-only, so a size match against what we already stored is
// a reliable "unchanged" signal and lets us skip re-reading multi-megabyte files.
func ScanAll(cfg *config.Config, db *index.DB, force bool) (*Result, error) {
	res := &Result{}
	matches, err := filepath.Glob(filepath.Join(cfg.ProjectsDir(), "*", "*.jsonl"))
	if err != nil {
		return res, err
	}

	for _, path := range matches {
		res.Scanned++
		uuid := strings.TrimSuffix(filepath.Base(path), ".jsonl")

		if !force {
			if st, err := os.Stat(path); err == nil {
				if known, _, ok := db.KnownTranscript(uuid); ok && known == st.Size() {
					// Unchanged transcript, but the directory may have been
					// deleted since — that transition is the whole point.
					_ = refreshDirState(db, uuid)
					res.Skipped++
					continue
				}
			}
		}

		if _, err := Ingest(cfg, db, path, "scan", true); err != nil {
			res.Failed++
			continue
		}
		res.Updated++
	}
	return res, nil
}

func refreshDirState(db *index.DB, uuid string) error {
	s, err := db.Get(uuid)
	if err != nil || s == nil {
		return err
	}
	want := index.DirMissing
	if dirExists(s.CWD) {
		want = index.DirOK
	}
	if want != s.DirState {
		return db.SetDirState(uuid, want)
	}
	return nil
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
