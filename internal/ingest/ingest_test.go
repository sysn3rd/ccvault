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
