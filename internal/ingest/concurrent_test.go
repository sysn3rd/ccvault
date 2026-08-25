package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
)

// This is the arrangement that actually happens in production and that nothing
// else tests: a capture hook fires inside a Claude session at the same moment
// the reconcile timer is scanning. They are separate processes with separate
// connections to one SQLite file. WAL mode plus busy_timeout is what makes that
// safe; this test is the evidence that it does.
func TestConcurrentCaptureAndScan(t *testing.T) {
	cfg, db := newEnv(t)
	defer db.Close()

	// Enough sessions that a scan takes long enough to overlap with writes.
	const sessions = 12
	dirs := make([]string, sessions)
	uuids := make([]string, sessions)
	for i := range sessions {
		dirs[i] = t.TempDir()
		if err := os.WriteFile(filepath.Join(dirs[i], "f.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		uuids[i] = fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)
		writeSession(t, cfg, "-p", uuids[i], dirs[i], fmt.Sprintf("session %d", i))
	}
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	// A second connection, as a separate process would have.
	hookDB, err := index.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer hookDB.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// The timer's reconcile pass, repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 8 {
			if _, err := ScanAll(cfg, db, true); err != nil {
				errs <- fmt.Errorf("scan: %w", err)
				return
			}
		}
	}()

	// Capture hooks firing on the other connection, concurrently.
	for i := range 4 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			path := filepath.Join(cfg.ProjectsDir(), "-p", uuids[n]+".jsonl")
			for range 8 {
				if _, err := Ingest(cfg, hookDB, path, "end", true); err != nil {
					errs <- fmt.Errorf("capture %d: %w", n, err)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		// "database is locked" here would mean busy_timeout is not doing its job
		// and a real capture could be lost.
		t.Errorf("concurrent access failed: %v", err)
	}

	// Every session must still be intact afterwards.
	for _, uuid := range uuids {
		s, err := db.Get(uuid)
		if err != nil {
			t.Fatalf("reading %s after concurrent access: %v", uuid[:8], err)
		}
		if s == nil {
			t.Errorf("session %s disappeared", uuid[:8])
		}
	}
}

// A hook and a scan both deciding to archive the same directory must not
// produce a corrupt or duplicated archive: snapshots are content-addressed, so
// they converge on one file.
func TestConcurrentSnapshotWritesConverge(t *testing.T) {
	cfg, db := newEnv(t)
	defer db.Close()

	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "notes.md"), []byte("shared content"), 0o600); err != nil {
		t.Fatal(err)
	}

	var uuids []string
	for i := range 6 {
		u := fmt.Sprintf("cc%06d-0000-0000-0000-000000000000", i)
		uuids = append(uuids, u)
		writeSession(t, cfg, "-shared", u, shared, "same directory")
	}

	var wg sync.WaitGroup
	for _, uuid := range uuids {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			conn, err := index.Open(cfg.DBPath())
			if err != nil {
				return
			}
			defer conn.Close()
			path := filepath.Join(cfg.ProjectsDir(), "-shared", u+".jsonl")
			_, _ = Ingest(cfg, conn, path, "end", true)
		}(uuid)
	}
	wg.Wait()

	entries, err := os.ReadDir(cfg.SnapshotsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("got %d archives for one directory, want 1: %v", len(entries), names)
	}
	for _, u := range uuids {
		snap, err := db.LatestSnapshot(u, index.SnapTree)
		if err != nil {
			t.Fatal(err)
		}
		if snap == nil {
			continue // a losing racer may legitimately have skipped
		}
		if _, err := os.Stat(snap.Path); err != nil {
			t.Errorf("%s points at a missing archive: %v", u[:8], err)
		}
	}
}

var _ = config.PendingDir
