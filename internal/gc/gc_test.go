package gc

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
)

func newEnv(t *testing.T) (*config.Config, *index.DB) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CCVAULT_CLAUDE_HOME", filepath.Join(root, "claude"))
	t.Setenv("CCVAULT_HOME", filepath.Join(root, "vault"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return cfg, db
}

func touch(t *testing.T, path string, size int) string {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A transcript is the one thing that cannot be regenerated. gc must never
// propose one that a session still references.
func TestNeverCollectsReferencedTranscripts(t *testing.T) {
	cfg, db := newEnv(t)

	tr := touch(t, filepath.Join(cfg.TranscriptsDir(), "keep.jsonl"), 100)
	if err := db.UpsertSession(&index.Session{
		UUID: "11111111-0000-0000-0000-000000000000", CWD: "/w", VaultPath: tr,
		LastActive: time.Now(),
	}, "body"); err != nil {
		t.Fatal(err)
	}

	p, err := Compute(cfg, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Candidates {
		if c.Path == tr {
			t.Fatalf("gc proposed deleting a referenced transcript: %+v", c)
		}
	}
	if _, err := Execute(db, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tr); err != nil {
		t.Errorf("referenced transcript was deleted: %v", err)
	}
}

// Whatever `forget` or a manual deletion leaves behind is exactly what gc is for.
func TestCollectsOrphanedFiles(t *testing.T) {
	cfg, db := newEnv(t)

	orphan := touch(t, filepath.Join(cfg.SnapshotsDir(), "nobody-points-here.tar.gz"), 512)
	orphanPatch := touch(t, filepath.Join(cfg.PatchesDir(), "stray.patch"), 64)

	p, err := Compute(cfg, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.FreedBytes != 576 {
		t.Errorf("FreedBytes = %d, want 576", p.FreedBytes)
	}

	r, err := Execute(db, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Removed != 2 {
		t.Errorf("Removed = %d, want 2", r.Removed)
	}
	for _, f := range []string{orphan, orphanPatch} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", filepath.Base(f))
		}
	}
}

// Every session start and end writes a git capture; without pruning they grow
// without bound.
func TestPrunesSupersededGitCaptures(t *testing.T) {
	cfg, db := newEnv(t)
	const uuid = "22222222-0000-0000-0000-000000000000"

	if err := db.UpsertSession(&index.Session{
		UUID: uuid, CWD: "/w", VaultPath: touch(t, filepath.Join(cfg.TranscriptsDir(), uuid+".jsonl"), 10),
		LastActive: time.Now(),
	}, "body"); err != nil {
		t.Fatal(err)
	}

	// Six captures, newest last.
	var paths []string
	for i := 0; i < 6; i++ {
		patch := touch(t, filepath.Join(cfg.PatchesDir(), filepathName(uuid, i)), 100)
		paths = append(paths, patch)
		if err := db.InsertGitState(&index.GitState{
			SessionUUID: uuid,
			CapturedAt:  time.Now().Add(time.Duration(i) * time.Minute),
			Event:       "end", PatchPath: patch,
		}); err != nil {
			t.Fatal(err)
		}
	}

	p, err := Compute(cfg, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(db, p); err != nil {
		t.Fatal(err)
	}

	// The three newest survive; the three oldest go.
	for i, path := range paths {
		_, err := os.Stat(path)
		if i < 3 && !os.IsNotExist(err) {
			t.Errorf("capture %d should have been pruned", i)
		}
		if i >= 3 && err != nil {
			t.Errorf("capture %d should have been kept: %v", i, err)
		}
	}

	// The newest capture must still resolve, or restore breaks.
	g, err := db.LatestGitState(uuid)
	if err != nil || g == nil {
		t.Fatal("latest git state disappeared")
	}
	if _, err := os.Stat(g.PatchPath); err != nil {
		t.Errorf("the capture a restore would use is gone: %v", err)
	}
}

// A row pointing at a file that has already vanished should be tidied, not
// reported as an error.
func TestCleansDanglingRows(t *testing.T) {
	cfg, db := newEnv(t)
	const uuid = "33333333-0000-0000-0000-000000000000"

	if err := db.UpsertSession(&index.Session{
		UUID: uuid, CWD: "/w", LastActive: time.Now(),
	}, "body"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := db.InsertGitState(&index.GitState{
			SessionUUID: uuid,
			CapturedAt:  time.Now().Add(time.Duration(i) * time.Minute),
			PatchPath:   filepath.Join(cfg.PatchesDir(), "never-existed.patch"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	p, err := Compute(cfg, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Dangling) == 0 {
		t.Fatal("expected dangling rows to be noticed")
	}
	r, err := Execute(db, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failed) != 0 {
		t.Errorf("a missing file should not be a failure: %v", r.Failed)
	}
}

// Age pruning must not override the retention count: the newest captures stay
// however old they are, because they are what a restore uses.
func TestAgePruningKeepsTheNewest(t *testing.T) {
	cfg, db := newEnv(t)
	const uuid = "44444444-0000-0000-0000-000000000000"

	if err := db.UpsertSession(&index.Session{
		UUID: uuid, CWD: "/w", LastActive: time.Now(),
	}, "body"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < 5; i++ {
		patch := touch(t, filepath.Join(cfg.PatchesDir(), filepathName(uuid, i)), 50)
		paths = append(paths, patch)
		if err := db.InsertGitState(&index.GitState{
			SessionUUID: uuid,
			CapturedAt:  time.Now().Add(-365 * 24 * time.Hour), // all ancient
			PatchPath:   patch,
		}); err != nil {
			t.Fatal(err)
		}
	}

	p, err := Compute(cfg, db, Options{OlderThan: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(db, p); err != nil {
		t.Fatal(err)
	}

	surviving := 0
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			surviving++
		}
	}
	if surviving != config.DefaultGitStatesKept {
		t.Errorf("%d captures survived, want %d — retention must outrank age", surviving, config.DefaultGitStatesKept)
	}
}

func filepathName(uuid string, i int) string {
	return uuid + "-" + string(rune('a'+i)) + ".patch"
}

// Retention is a setting, not a constant: the settings file must actually
// change how much history is kept.
func TestRetentionComesFromSettings(t *testing.T) {
	cfg, db := newEnv(t)
	cfg.Retention.GitStates = 1 // keep only the newest
	const uuid = "55555555-0000-0000-0000-000000000000"

	if err := db.UpsertSession(&index.Session{
		UUID: uuid, CWD: "/w", LastActive: time.Now(),
	}, "body"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := 0; i < 4; i++ {
		p := touch(t, filepath.Join(cfg.PatchesDir(), uuid+"-"+string(rune('a'+i))+".patch"), 10)
		paths = append(paths, p)
		if err := db.InsertGitState(&index.GitState{
			SessionUUID: uuid,
			CapturedAt:  time.Now().Add(time.Duration(i) * time.Minute),
			PatchPath:   p,
		}); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := Compute(cfg, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(db, plan); err != nil {
		t.Fatal(err)
	}

	surviving := 0
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			surviving++
		}
	}
	if surviving != 1 {
		t.Errorf("%d captures survived with git_states_kept=1, want 1", surviving)
	}
}
