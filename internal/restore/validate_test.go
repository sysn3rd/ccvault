package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sysn3rd/ccvault/internal/index"
)

// A remote beginning with "-" is read by git as an option, not a URL.
// "--upload-pack=<command>" is arbitrary command execution during a clone, and
// the remote is whatever was in the .git/config of a directory you opened.
func TestRefusesRemoteThatGitWouldReadAsAnOption(t *testing.T) {
	hostile := []string{
		"--upload-pack=touch /tmp/pwned",
		"--config=core.pager=touch /tmp/pwned",
		"-u touch /tmp/pwned",
	}
	for _, remote := range hostile {
		t.Run(remote, func(t *testing.T) {
			err := validateGitInputs(remote, "main", "abc1234")
			if err == nil {
				t.Fatalf("accepted a hostile remote: %q", remote)
			}
			if !strings.Contains(err.Error(), "command-line option") {
				t.Errorf("error should explain the danger, got: %v", err)
			}
		})
	}
}

// Prepare must refuse before anything is written, not fail part-way through.
func TestPrepareRefusesHostileRemoteBeforeWriting(t *testing.T) {
	db := newDB(t)
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "aaaa0000-0000-0000-0000-000000000000", target, "/vault/t.jsonl")
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(),
		RemoteURL: "--upload-pack=touch /tmp/ccvault-should-never-exist",
		Branch:    "main", HeadSHA: "0123456789abcdef0123456789abcdef01234567",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Prepare(db, s, Options{}); err == nil {
		t.Fatal("Prepare accepted a hostile remote")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("nothing should have been written")
	}
}

func TestRefusesImplausibleValues(t *testing.T) {
	cases := []struct{ name, remote, branch, sha string }{
		{"branch as option", "https://example.com/r.git", "--exec=evil", "abc1234"},
		{"sha not hex", "https://example.com/r.git", "main", "; rm -rf /"},
		{"newline in remote", "https://example.com/\nrm -rf /", "main", "abc1234"},
		{"remote is nonsense", "not a url at all", "main", "abc1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validateGitInputs(c.remote, c.branch, c.sha); err == nil {
				t.Errorf("accepted %+v", c)
			}
		})
	}
}

// The forms people actually use must all still pass.
func TestAcceptsRealWorldRemotes(t *testing.T) {
	remotes := []string{
		"git@github.com:example/repo.git",
		"https://github.com/example/repo.git",
		"ssh://git@git.example.com:2222/team/repo.git",
		"file:///srv/git/repo.git",
		"/srv/git/repo.git",
		"../sibling-repo",
		"git://example.com/repo.git",
	}
	for _, r := range remotes {
		t.Run(r, func(t *testing.T) {
			if err := validateGitInputs(r, "feature/thing_1.2", "a2ac4b91aa52edb9c09cf4b93300940f8eee38b4"); err != nil {
				t.Errorf("rejected a legitimate remote: %v", err)
			}
		})
	}
	// A detached capture records HEAD rather than a branch name.
	if err := validateGitInputs("git@github.com:example/repo.git", "HEAD", "a2ac4b91"); err != nil {
		t.Errorf("rejected a detached-HEAD capture: %v", err)
	}
	// And a session with no git state at all validates trivially.
	if err := validateGitInputs("", "", ""); err != nil {
		t.Errorf("rejected an empty capture: %v", err)
	}
}

// "--" means "paths follow" to git checkout, so it must not be used with a
// revision. This test exists because adding it there broke detached checkout
// and no other test covered that path.
func TestRestoreDetachedHeadCheckout(t *testing.T) {
	db := newDB(t)
	remote, sha := newRemote(t)
	target := filepath.Join(t.TempDir(), "rebuilt")
	s := addSession(t, db, "bbbb0000-0000-0000-0000-000000000000", target, "/vault/t.jsonl")

	// Branch "HEAD" is what a detached capture records.
	if err := db.InsertGitState(&index.GitState{
		SessionUUID: s.UUID, CapturedAt: time.Now(),
		RemoteURL: remote, Branch: "HEAD", HeadSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := Prepare(db, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Execute(plan)
	if err != nil {
		t.Fatalf("detached checkout failed: %v", err)
	}
	for _, w := range report.Warnings {
		if strings.Contains(w, "never pushed") {
			t.Fatalf("the commit should have been found: %v", report.Warnings)
		}
	}
	if head := runGit(t, target, "rev-parse", "HEAD"); head != sha {
		t.Errorf("HEAD = %s, want %s", head, sha)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("clone contents missing: %v", err)
	}
}
