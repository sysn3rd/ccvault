package restore

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/snapshot"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newDB(t *testing.T) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func addSession(t *testing.T, db *index.DB, uuid, cwd string, vaultPath string) *index.Session {
	t.Helper()
	s := &index.Session{
		UUID: uuid, CWD: cwd, CWDs: []string{cwd}, Title: "test session",
		Kind: index.KindRepo, DirState: index.DirMissing,
		VaultPath: vaultPath, LastActive: time.Now(),
	}
	if err := db.UpsertSession(s, "body"); err != nil {
		t.Fatal(err)
	}
	return s
}

// newRemote builds a bare repo with one commit and returns (remoteURL, sha).
func newRemote(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "origin.git")
	runGit(t, root, "init", "-q", "--bare", "-b", "main", bare)

	work := filepath.Join(root, "work")
	runGit(t, root, "clone", "-q", bare, work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("committed content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-qm", "initial")
	runGit(t, work, "push", "-q", "origin", "main")
	sha := runGit(t, work, "rev-parse", "HEAD")
	return bare, sha
}

func TestRestoreClonesAtRecordedCommit(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "11111111-1111-1111-1111-111111111111", target, "/vault/t.jsonl")

	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), Event: "end",
		RemoteURL: remote, Branch: "main", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategyClone {
		t.Fatalf("Strategy = %q, want clone", plan.Strategy)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "committed content\n" {
		t.Errorf("README.md = %q", got)
	}
	if head := runGit(t, target, "rev-parse", "HEAD"); head != sha {
		t.Errorf("HEAD = %s, want %s", head, sha)
	}
	if branch := runGit(t, target, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Errorf("branch = %s, want main", branch)
	}
}

// The dirty patch and untracked bundle are what make a restore faithful rather
// than merely "the right commit".
func TestRestoreReplaysUncommittedWork(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	vault := t.TempDir()
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "22222222-2222-2222-2222-222222222222", target, "/vault/t.jsonl")

	patch := `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1,2 @@
 committed content
+uncommitted line
`
	patchPath := filepath.Join(vault, "p.patch")
	if err := os.WriteFile(patchPath, []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}

	// An untracked bundle, in the same format capture writes.
	scratch := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratch, "scratch.txt"), []byte("untracked work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Plan(scratch, snapshot.Options{})
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(vault, "u.tar.gz")
	if _, err := snapshot.Write(m, bundlePath); err != nil {
		t.Fatal(err)
	}

	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), Event: "end",
		RemoteURL: remote, Branch: "main", HeadSHA: sha, IsDirty: true,
		PatchPath: patchPath, UntrackedPath: bundlePath,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Execute(plan)
	if err != nil {
		t.Fatal(err)
	}

	readme, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "uncommitted line") {
		t.Errorf("patch not replayed; README.md = %q (warnings: %v)", readme, report.Warnings)
	}
	scratchOut, err := os.ReadFile(filepath.Join(target, "scratch.txt"))
	if err != nil {
		t.Fatalf("untracked file not restored: %v", err)
	}
	if string(scratchOut) != "untracked work\n" {
		t.Errorf("scratch.txt = %q", scratchOut)
	}
}

// A commit that was never pushed simply is not on the remote. That must be
// reported plainly, not surface as a raw git failure.
func TestRestoreWarnsOnUnpushedCommit(t *testing.T) {
	db := newDB(t)
	remote, _ := newRemote(t)
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "33333333-3333-3333-3333-333333333333", target, "/vault/t.jsonl")

	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), Event: "end",
		RemoteURL: remote, Branch: "main",
		HeadSHA:   "0000000000000000000000000000000000000000",
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Execute(plan)
	if err != nil {
		t.Fatalf("an unpushed commit should warn, not fail: %v", err)
	}
	var found bool
	for _, w := range report.Warnings {
		if strings.Contains(w, "never pushed") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one about the commit not being on the remote", report.Warnings)
	}
	// The clone is still there and usable.
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("clone should still be usable: %v", err)
	}
}

func TestRestoreFromSnapshot(t *testing.T) {
	db := newDB(t)
	vault := t.TempDir()
	target := filepath.Join(t.TempDir(), "rebuilt")

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "notes.md"), []byte("scratch workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := snapshot.Plan(src, snapshot.Options{})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(vault, "snap.tar.gz")
	size, err := snapshot.Write(m, archive)
	if err != nil {
		t.Fatal(err)
	}

	s := addSession(t, db, "44444444-4444-4444-4444-444444444444", target, "/vault/t.jsonl")
	if err := db.InsertSnapshot(&index.Snapshot{
		SessionUUID: s.UUID, CapturedAt: time.Now(), Kind: index.SnapTree,
		Path: archive, Bytes: size, TreeHash: m.TreeHash, FileCount: len(m.Files),
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Strategy != StrategySnapshot {
		t.Fatalf("Strategy = %q, want snapshot", plan.Strategy)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "scratch workspace" {
		t.Errorf("notes.md = %q", got)
	}
}

// Overwriting a directory that already holds work is the one truly destructive
// thing a restore could do.
func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "precious.txt"), []byte("do not lose me"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := addSession(t, db, "55555555-5555-5555-5555-555555555555", target, "/vault/t.jsonl")
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), RemoteURL: remote, Branch: "main", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Prepare(db, s, Options{}); err == nil {
		t.Fatal("expected a refusal for a non-empty target")
	} else if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force, got: %v", err)
	}

	// The file must still be there after the refusal.
	if _, err := os.Stat(filepath.Join(target, "precious.txt")); err != nil {
		t.Errorf("existing content was touched: %v", err)
	}
}

// An empty directory is the ~/code/games case and needs no --force.
func TestRestoreAcceptsEmptyTarget(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	target := t.TempDir() // exists, empty
	s := addSession(t, db, "66666666-6666-6666-6666-666666666666", target, "/vault/t.jsonl")
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), RemoteURL: remote, Branch: "main", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatalf("an empty directory should be restorable in place: %v", err)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("clone did not land: %v", err)
	}
}

// A session ccvault never saw while its directory existed cannot be rebuilt,
// and saying so clearly beats a confusing failure later.
func TestPrepareExplainsWhenNothingWasCaptured(t *testing.T) {
	db := newDB(t)
	s := addSession(t, db, "77777777-7777-7777-7777-777777777777",
		filepath.Join(t.TempDir(), "never-captured"), "/vault/t.jsonl")

	_, err := Prepare(db, s, Options{})
	if err == nil {
		t.Fatal("expected an error when there is nothing to restore from")
	}
	if !strings.Contains(err.Error(), "no remote and no archive") {
		t.Errorf("error should explain why, got: %v", err)
	}
}

// A pruned transcript still restores the directory; it just cannot resume.
func TestPrepareWarnsWhenTranscriptIsPruned(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "88888888-8888-8888-8888-888888888888", target, "") // no vault path
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), RemoteURL: remote, Branch: "main", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, w := range plan.Warnings {
		if strings.Contains(w, "cannot be resumed") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one about the pruned transcript", plan.Warnings)
	}
}

func TestRestoreToAlternateTarget(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	original := filepath.Join(t.TempDir(), "original")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")

	s := addSession(t, db, "99999999-9999-9999-9999-999999999999", original, "/vault/t.jsonl")
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(), RemoteURL: remote, Branch: "main", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{Target: elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target != elsewhere {
		t.Errorf("Target = %q, want %q", plan.Target, elsewhere)
	}
	if _, err := Execute(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "README.md")); err != nil {
		t.Errorf("did not restore to the alternate target: %v", err)
	}
	if _, err := os.Stat(original); !os.IsNotExist(err) {
		t.Error("the original path should have been left alone")
	}
}
