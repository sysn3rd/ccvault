package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
)

// newEnv builds an isolated Claude home + vault so tests never touch the real one.
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
	if err := os.MkdirAll(cfg.ProjectsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := index.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return cfg, db
}

func writeSession(t *testing.T, cfg *config.Config, slug, uuid, cwd, text string) {
	t.Helper()
	dir := filepath.Join(cfg.ProjectsDir(), slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","sessionId":"` + uuid + `","cwd":"` + cwd +
		`","timestamp":"2026-08-01T10:00:00.000Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, uuid+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSlugCollisionKeepsSessionsDistinct is the sharpest edge in the whole
// design. Claude Code maps /, . and _ all to - when naming a project folder, so
// two different directories share one folder and their transcripts interleave.
// Identity must come from the session UUID and location from the cwd recorded
// inside the transcript — never from the folder name.
func TestSlugCollisionKeepsSessionsDistinct(t *testing.T) {
	cfg, db := newEnv(t)

	const shared = "-home-u-code-a-b-c" // what both of these collapse to
	writeSession(t, cfg, shared, "11111111-1111-1111-1111-111111111111", "/home/u/code/a.b_c", "dotted underscore repo")
	writeSession(t, cfg, shared, "22222222-2222-2222-2222-222222222222", "/home/u/code/a-b-c", "hyphenated repo")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	sessions, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2 — the collision collapsed them", len(sessions))
	}

	byUUID := map[string]string{}
	for _, s := range sessions {
		byUUID[s.UUID] = s.CWD
	}
	want := map[string]string{
		"11111111-1111-1111-1111-111111111111": "/home/u/code/a.b_c",
		"22222222-2222-2222-2222-222222222222": "/home/u/code/a-b-c",
	}
	for uuid, wantCWD := range want {
		if got := byUUID[uuid]; got != wantCWD {
			t.Errorf("session %s cwd = %q, want %q", uuid[:8], got, wantCWD)
		}
	}
}

// A directory that no longer exists is the whole reason this tool exists.
func TestScanMarksMissingDirectories(t *testing.T) {
	cfg, db := newEnv(t)

	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSession(t, cfg, "-live", "33333333-3333-3333-3333-333333333333", live, "still here")
	writeSession(t, cfg, "-gone", "44444444-4444-4444-4444-444444444444", "/home/u/code/deleted-project", "long gone")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	sessions, err := db.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		got[s.UUID[:8]] = s.DirState
	}
	if got["33333333"] != index.DirOK {
		t.Errorf("live directory marked %q, want ok", got["33333333"])
	}
	if got["44444444"] != index.DirMissing {
		t.Errorf("deleted directory marked %q, want missing", got["44444444"])
	}
}

// Transcripts are append-only, so an unchanged file must not be re-read; but a
// directory deleted since the last scan must still be noticed.
func TestScanSkipsUnchangedButStillRefreshesDirState(t *testing.T) {
	cfg, db := newEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSession(t, cfg, "-p", "55555555-5555-5555-5555-555555555555", dir, "hello")

	res, err := ScanAll(cfg, db, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 || res.Skipped != 0 {
		t.Fatalf("first scan: updated=%d skipped=%d, want 1/0", res.Updated, res.Skipped)
	}

	// Nothing changed on disk: the transcript should be skipped entirely.
	res, err = ScanAll(cfg, db, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 1 || res.Updated != 0 {
		t.Fatalf("second scan: updated=%d skipped=%d, want 0/1", res.Updated, res.Skipped)
	}

	// Now delete the directory. The transcript is still unchanged, so it is
	// skipped again — but dir_state must flip anyway.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}
	s, err := db.Get("55555555-5555-5555-5555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if s.DirState != index.DirMissing {
		t.Errorf("dir_state = %q after deletion, want missing", s.DirState)
	}
}

// The vault must hold its own copy; losing ~/.claude must not lose context.
func TestIngestCopiesTranscriptIntoVault(t *testing.T) {
	cfg, db := newEnv(t)
	writeSession(t, cfg, "-p", "66666666-6666-6666-6666-666666666666", t.TempDir(), "keep me")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}
	s, err := db.Get("66666666-6666-6666-6666-666666666666")
	if err != nil {
		t.Fatal(err)
	}
	if s.VaultPath == "" {
		t.Fatal("VaultPath is empty")
	}
	data, err := os.ReadFile(s.VaultPath)
	if err != nil {
		t.Fatalf("vault copy unreadable: %v", err)
	}
	if len(data) == 0 {
		t.Error("vault copy is empty")
	}
	info, err := os.Stat(s.VaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("vault copy mode = %o, want 600 (transcripts hold secrets)", perm)
	}
}

// The index holds prompt text and the full-text body; it must be no more
// readable than the transcripts it was built from.
func TestIndexIsOwnerOnly(t *testing.T) {
	cfg, db := newEnv(t)
	writeSession(t, cfg, "-p", "77777777-7777-7777-7777-777777777777", t.TempDir(), "sensitive prompt")
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(cfg.DBPath() + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(cfg.DBPath()+suffix), perm)
		}
	}
}

func writeHistory(t *testing.T, cfg *config.Config, lines ...string) {
	t.Helper()
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(cfg.HistoryFile(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Claude Code prunes transcripts after cleanupPeriodDays but keeps the prompt
// log forever. Those sessions must stay findable, flagged as having no context.
func TestSeedFromHistoryRecoversPrunedSessions(t *testing.T) {
	cfg, db := newEnv(t)
	writeHistory(t, cfg,
		`{"display":"how do I play an audiobook from USB","timestamp":1787499114709,"project":"/home/u/Work","sessionId":"aaaa1111-0000-0000-0000-000000000000"}`,
		`{"display":"try mpv instead","timestamp":1787499514709,"project":"/home/u/Work","sessionId":"aaaa1111-0000-0000-0000-000000000000"}`,
	)

	res, err := ScanAll(cfg, db, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seeded != 1 {
		t.Fatalf("Seeded = %d, want 1", res.Seeded)
	}

	got, err := db.Search("audiobook")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("search returned %d results, want 1", len(got))
	}
	s := got[0]
	if s.HasTranscript() {
		t.Error("HasTranscript() = true for a prompt-log-only session")
	}
	if s.MsgCount != 2 {
		t.Errorf("MsgCount = %d, want 2", s.MsgCount)
	}
	if s.CWD != "/home/u/Work" {
		t.Errorf("CWD = %q", s.CWD)
	}
	// The second prompt must be searchable too, not just the first.
	if got, err := db.Search("mpv"); err != nil || len(got) != 1 {
		t.Errorf("second prompt not indexed (got %d, err %v)", len(got), err)
	}
}

// A transcript is strictly richer; the prompt log must never overwrite it.
func TestSeedFromHistoryDoesNotClobberTranscripts(t *testing.T) {
	cfg, db := newEnv(t)
	const uuid = "bbbb2222-0000-0000-0000-000000000000"
	dir := t.TempDir()
	writeSession(t, cfg, "-p", uuid, dir, "the real transcript text")
	writeHistory(t, cfg,
		`{"display":"the real transcript text","timestamp":1787499114709,"project":"`+dir+`","sessionId":"`+uuid+`"}`,
	)

	res, err := ScanAll(cfg, db, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seeded != 0 {
		t.Errorf("Seeded = %d, want 0 — the transcript already covers it", res.Seeded)
	}
	s, err := db.Get(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasTranscript() {
		t.Error("transcript-backed session lost its vault copy")
	}
}

func TestGitRefRendering(t *testing.T) {
	cases := []struct {
		branch, sha, want string
	}{
		{"main", "a2ac4b91aa52edb9c09cf4b93300940f8eee38b4", "main@a2ac4b91"},
		{"main", "", "main"},
		{"", "", ""},
	}
	for _, c := range cases {
		s := &index.Session{Branch: c.branch, HeadSHA: c.sha}
		if got := s.GitRef(); got != c.want {
			t.Errorf("GitRef(%q,%q) = %q, want %q", c.branch, c.sha, got, c.want)
		}
	}
}

// A directory that exists but holds nothing is not the same as a healthy one:
// resuming into it lands you somewhere useless. ~/code/games on this machine is
// exactly this case.
func TestScanDistinguishesEmptyFromOK(t *testing.T) {
	cfg, db := newEnv(t)

	empty := t.TempDir()
	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSession(t, cfg, "-e", "88888888-8888-8888-8888-888888888888", empty, "empty dir session")
	writeSession(t, cfg, "-f", "99999999-9999-9999-9999-999999999999", populated, "populated dir session")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	e, err := db.Get("88888888-8888-8888-8888-888888888888")
	if err != nil {
		t.Fatal(err)
	}
	if e.DirState != index.DirEmpty {
		t.Errorf("empty directory marked %q, want %q", e.DirState, index.DirEmpty)
	}
	p, err := db.Get("99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	if p.DirState != index.DirOK {
		t.Errorf("populated directory marked %q, want ok", p.DirState)
	}
}

// A plain directory has no git safety net, so the archive is the only thing
// that makes it restorable.
func TestScanSnapshotsPlainDirectories(t *testing.T) {
	cfg, db := newEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSession(t, cfg, "-p", "aaaabbbb-0000-0000-0000-000000000000", dir, "plain work")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	snap, err := db.LatestSnapshot("aaaabbbb-0000-0000-0000-000000000000", index.SnapTree)
	if err != nil {
		t.Fatal(err)
	}
	if snap == nil {
		t.Fatal("no tree snapshot taken for a plain directory")
	}
	if snap.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", snap.FileCount)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Errorf("snapshot file missing: %v", err)
	}
}

// An idle directory must not accumulate an archive per capture event.
func TestSnapshotSkippedWhenUnchanged(t *testing.T) {
	cfg, db := newEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("stable"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ProjectsDir(), "-p", "bbbbcccc-0000-0000-0000-000000000000.jsonl")
	writeSession(t, cfg, "-p", "bbbbcccc-0000-0000-0000-000000000000", dir, "plain work")

	for i := 0; i < 3; i++ {
		if _, err := Ingest(cfg, db, path, "end", true); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(cfg.SnapshotsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("got %d archives after 3 unchanged captures, want 1", len(entries))
	}
}

// The picker and the plain listing must never disagree about a session's health.
func TestStateLabel(t *testing.T) {
	cases := []struct {
		name     string
		s        index.Session
		want     string
		needsFix bool
	}{
		{"healthy", index.Session{VaultPath: "/v/x.jsonl", DirState: index.DirOK}, "OK", false},
		{"deleted", index.Session{VaultPath: "/v/x.jsonl", DirState: index.DirMissing}, "MISSING", true},
		{"emptied", index.Session{VaultPath: "/v/x.jsonl", DirState: index.DirEmpty}, "EMPTY", true},
		// A pruned transcript outranks directory health: there is no context
		// to resume either way.
		{"pruned", index.Session{VaultPath: "", DirState: index.DirOK}, "NO TRANSCRIPT", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.StateLabel(); got != c.want {
				t.Errorf("StateLabel() = %q, want %q", got, c.want)
			}
			if got := c.s.NeedsRestore(); got != c.needsFix {
				t.Errorf("NeedsRestore() = %v, want %v", got, c.needsFix)
			}
		})
	}
}

// Many sessions share one working directory — every session in ~/Work, for
// instance. Naming archives by content means they share one file rather than
// storing N identical copies.
func TestSnapshotsAreSharedAcrossSessions(t *testing.T) {
	cfg, db := newEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.md"), []byte("same content"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{
		"cccc1111-0000-0000-0000-000000000000",
		"cccc2222-0000-0000-0000-000000000000",
		"cccc3333-0000-0000-0000-000000000000",
	} {
		writeSession(t, cfg, "-shared", uuid, dir, "session in the shared dir")
	}
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(cfg.SnapshotsDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("3 sessions sharing a directory produced %d archives, want 1", len(entries))
	}

	// Every session must still resolve to a usable archive.
	for _, uuid := range []string{
		"cccc1111-0000-0000-0000-000000000000",
		"cccc3333-0000-0000-0000-000000000000",
	} {
		snap, err := db.LatestSnapshot(uuid, index.SnapTree)
		if err != nil || snap == nil {
			t.Fatalf("%s has no snapshot row", uuid[:8])
		}
		if _, err := os.Stat(snap.Path); err != nil {
			t.Errorf("%s points at a missing archive: %v", uuid[:8], err)
		}
	}
}

// Forgetting one session must not delete an archive another still needs.
func TestForgetKeepsSharedSnapshots(t *testing.T) {
	cfg, db := newEnv(t)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shared.md"), []byte("same content"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := "dddd1111-0000-0000-0000-000000000000"
	drop := "dddd2222-0000-0000-0000-000000000000"
	writeSession(t, cfg, "-shared", keep, dir, "keep this one")
	writeSession(t, cfg, "-shared", drop, dir, "drop this one")
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}

	kept, err := db.LatestSnapshot(keep, index.SnapTree)
	if err != nil || kept == nil {
		t.Fatal("no snapshot for the session being kept")
	}

	files, err := db.Forget(drop)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == kept.Path {
			t.Fatal("Forget offered up an archive that another session still references")
		}
		os.Remove(f)
	}
	if _, err := os.Stat(kept.Path); err != nil {
		t.Errorf("the surviving session's archive was deleted: %v", err)
	}

	// And the forgotten session is really gone.
	if s, err := db.Get(drop); err != nil || s != nil {
		t.Error("forgotten session is still in the index")
	}
}

// A repository whose remote already covers it needs no archive — and that
// answer must be remembered. Otherwise every reconcile re-decides it, shelling
// out to git each time, forever.
func TestSnapshotDecisionIsRememberedForSessionsNeedingNoArchive(t *testing.T) {
	cfg, db := newEnv(t)

	// A plain directory with nothing in it: snapshotPlan wants an archive but
	// there is nothing to put in one, so no snapshot row is ever created.
	empty := t.TempDir()
	const uuid = "ffff0000-0000-0000-0000-000000000000"
	writeSession(t, cfg, "-p", uuid, empty, "session in an empty directory")

	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}
	if !db.SnapshotChecked(uuid) {
		t.Fatal("the decision was not recorded, so it will be retaken every scan")
	}
	if snap, _ := db.LatestSnapshot(uuid, index.SnapTree); snap != nil {
		t.Error("an empty directory should not produce an archive")
	}

	// A later reconcile must short-circuit rather than re-evaluate.
	if _, err := ScanAll(cfg, db, false); err != nil {
		t.Fatal(err)
	}
	if !db.SnapshotChecked(uuid) {
		t.Error("the recorded decision was lost")
	}
}
