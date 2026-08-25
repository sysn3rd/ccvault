package ingest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/pending"
)

func gitInit(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("remote", "add", "origin", "git@github.com:example/held.git")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "-qm", "initial")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:40])
}

// A hook cannot prompt for a new vault location, and skipping would throw away
// the commit SHA and the diff — the two things no later scan can reconstruct.
func TestDeferredCaptureKeepsGitProvenance(t *testing.T) {
	cfg, db := newEnv(t)
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	work := t.TempDir()
	sha := gitInit(t, work)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const uuid = "eeee1111-0000-0000-0000-000000000000"
	writeSession(t, cfg, "-p", uuid, work, "session while the vault was away")
	tpath := filepath.Join(cfg.ProjectsDir(), "-p", uuid+".jsonl")

	if err := CaptureDeferred(cfg, tpath, "end"); err != nil {
		t.Fatal(err)
	}

	dir, err := config.PendingDir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := pending.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d held captures, want 1", len(records))
	}
	r := records[0]
	if r.Git == nil {
		t.Fatal("no git provenance was held")
	}
	if r.Git.HeadSHA != sha {
		t.Errorf("HeadSHA = %q, want %q", r.Git.HeadSHA, sha)
	}
	if r.Git.RemoteURL != "git@github.com:example/held.git" {
		t.Errorf("RemoteURL = %q", r.Git.RemoteURL)
	}
	if r.Git.PatchFile == "" {
		t.Error("the uncommitted diff was not held")
	}
	if r.Git.UntrackedFile == "" {
		t.Error("untracked files were not held")
	}

	// Now the vault is back: adopting must land everything in it.
	res, err := Adopt(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 1 || len(res.Failed) != 0 {
		t.Fatalf("Adopted = %d, Failed = %v", res.Adopted, res.Failed)
	}

	g, err := db.LatestGitState(uuid)
	if err != nil || g == nil {
		t.Fatal("no git state after adoption")
	}
	if g.HeadSHA != sha {
		t.Errorf("adopted HeadSHA = %q, want %q", g.HeadSHA, sha)
	}
	if _, err := os.Stat(g.PatchPath); err != nil {
		t.Errorf("adopted patch is not in the vault: %v", err)
	}
	if _, err := os.Stat(g.UntrackedPath); err != nil {
		t.Errorf("adopted untracked bundle is not in the vault: %v", err)
	}

	s, err := db.Get(uuid)
	if err != nil || s == nil {
		t.Fatal("session was not indexed on adoption")
	}
	if !s.HasTranscript() {
		t.Error("adopted session has no transcript in the vault")
	}
	if s.Kind != index.KindRepo {
		t.Errorf("Kind = %q, want repo", s.Kind)
	}

	// And the holding area is emptied once its contents are safely filed.
	if n := store.Count(); n != 0 {
		t.Errorf("%d records still held after adoption", n)
	}
}

// Adoption must be idempotent enough that a second run is a no-op rather than
// a duplicate.
func TestAdoptWithNothingHeldIsHarmless(t *testing.T) {
	cfg, db := newEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	res, err := Adopt(cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	if res.Adopted != 0 || len(res.Failed) != 0 {
		t.Errorf("Adopted = %d, Failed = %v", res.Adopted, res.Failed)
	}
}
