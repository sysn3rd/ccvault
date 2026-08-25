package gitstate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "remote", "add", "origin", "git@github.com:example/repo.git")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "README.md")
	git(t, dir, "commit", "-qm", "initial")
	return dir
}

func TestCaptureCleanRepo(t *testing.T) {
	dir := newRepo(t)

	s, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsRepo {
		t.Fatal("IsRepo = false for a git worktree")
	}
	if s.RemoteURL != "git@github.com:example/repo.git" {
		t.Errorf("RemoteURL = %q", s.RemoteURL)
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want main", s.Branch)
	}
	if len(s.HeadSHA) != 40 {
		t.Errorf("HeadSHA = %q, want a full sha", s.HeadSHA)
	}
	if s.IsDirty {
		t.Error("IsDirty = true on a clean tree")
	}
	if s.Patch != "" {
		t.Error("clean tree should produce no patch")
	}
}

// The dirty patch and untracked list are what make a restore faithful rather
// than merely "the right commit".
func TestCaptureDirtyRepo(t *testing.T) {
	dir := newRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\nmodified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsDirty {
		t.Fatal("IsDirty = false with a modified file")
	}
	if s.Patch == "" {
		t.Error("no patch captured for a modified tracked file")
	}
	if len(s.Untracked) != 1 || s.Untracked[0] != "scratch.txt" {
		t.Errorf("Untracked = %v, want [scratch.txt]", s.Untracked)
	}
}

// A clone-based restore silently loses an untracked .claude/, taking the
// project's skills and agents with it — so we must know which case we are in.
func TestCaptureDetectsClaudeDirTracking(t *testing.T) {
	dir := newRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude", "skills", "s.md"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.ClaudeDirTracked {
		t.Error("ClaudeDirTracked = true while .claude is untracked")
	}

	git(t, dir, "add", ".claude")
	git(t, dir, "commit", "-qm", "add claude dir")

	s, err = Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ClaudeDirTracked {
		t.Error("ClaudeDirTracked = false after committing .claude")
	}
}

// ~/Work is not a repo, and that is the majority case. It must be a normal
// answer, not an error.
func TestCaptureNonRepoIsNotAnError(t *testing.T) {
	s, err := Capture(t.TempDir())
	if err != nil {
		t.Fatalf("non-repo directory returned an error: %v", err)
	}
	if s.IsRepo {
		t.Error("IsRepo = true for a plain directory")
	}
}

// A capture hook runs inside a live Claude session, so a git that hangs — on a
// lock, on an unreachable remote — must not hang the session with it.
func TestGitCallsAreBounded(t *testing.T) {
	dir := t.TempDir()
	hang := filepath.Join(dir, "hanging-git")
	script := "#!/bin/sh\nsleep 60\n"
	if err := os.WriteFile(hang, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	prevBinary, prevTimeout := gitBinary, cmdTimeout
	gitBinary = hang
	cmdTimeout = 300 * time.Millisecond
	t.Cleanup(func() { gitBinary, cmdTimeout = prevBinary, prevTimeout })

	start := time.Now()
	_, err := run(dir, "status")
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a command that never returns should produce an error")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("git call took %v — it is not bounded by the timeout", elapsed)
	}
}
