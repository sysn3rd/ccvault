// Package gitstate captures the git provenance Claude Code never records.
//
// Transcripts store cwd and a bare branch name and nothing else — no remote, no
// commit. Without the remote URL and SHA captured at session time, a deleted
// repository cannot be rebuilt, so this runs on every capture event.
package gitstate

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// State is the git provenance of a working directory at one moment.
// A zero IsRepo means the directory is not a git worktree, which is the normal
// case for scratch workspaces like ~/Work.
type State struct {
	IsRepo       bool
	WorktreeRoot string
	RemoteURL    string
	Branch       string
	HeadSHA      string
	IsDirty      bool

	// Patch is `git diff HEAD` — tracked-file modifications, replayable on restore.
	Patch string
	// Untracked lists files git does not know about, relative to the worktree root.
	// They are not in Patch and would otherwise be lost.
	Untracked  []string
	Submodules []string

	// ClaudeDirTracked reports whether .claude/ is committed. When it is not,
	// a clone-based restore silently comes back without the project's skills
	// and agents, so the caller must snapshot it separately.
	ClaudeDirTracked bool
	ClaudeDirExists  bool
}

// cmdTimeout bounds every git call. A variable rather than a constant so the
// timeout itself can be tested.
var cmdTimeout = 10 * time.Second

// Capture inspects dir. It never returns an error for "not a repo" — that is a
// valid answer. Errors are reserved for git being unusable entirely.
func Capture(dir string) (*State, error) {
	s := &State{}

	root, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return s, nil // not a worktree; caller falls back to a tree snapshot
	}
	s.IsRepo = true
	s.WorktreeRoot = root

	s.Branch, _ = run(dir, "rev-parse", "--abbrev-ref", "HEAD")
	s.HeadSHA, _ = run(dir, "rev-parse", "HEAD")

	// Prefer origin, but take any remote over none — a repo cloned under a
	// different remote name is still fully restorable.
	if u, err := run(dir, "remote", "get-url", "origin"); err == nil && u != "" {
		s.RemoteURL = u
	} else if names, _ := run(dir, "remote"); names != "" {
		first := strings.Fields(names)[0]
		s.RemoteURL, _ = run(dir, "remote", "get-url", first)
	}

	if status, err := run(dir, "status", "--porcelain"); err == nil && status != "" {
		s.IsDirty = true
	}
	if s.IsDirty {
		// Force canonical a/ and b/ prefixes and inline binary diffs. Without
		// this the patch inherits local git config (diff.mnemonicPrefix emits
		// c/ and w/), which may not apply on another machine.
		// runRaw, not run: a patch must keep its exact bytes. Trimming the
		// trailing newline makes git reject the whole thing as "corrupt patch".
		s.Patch, _ = runRaw(dir,
			"-c", "diff.mnemonicPrefix=false",
			"-c", "diff.noprefix=false",
			"diff", "--binary", "HEAD")
		if out, err := run(dir, "ls-files", "--others", "--exclude-standard"); err == nil && out != "" {
			s.Untracked = strings.Split(out, "\n")
		}
	}
	if out, err := run(dir, "submodule", "status"); err == nil && out != "" {
		s.Submodules = strings.Split(out, "\n")
	}

	if out, err := run(dir, "ls-files", ".claude"); err == nil && out != "" {
		s.ClaudeDirTracked = true
	}

	return s, nil
}

// gitBinary is a variable so tests can substitute a command that misbehaves;
// nothing else should change it.
var gitBinary = "git"

// runRaw returns git's output byte for byte. Used for patches, where trailing
// whitespace is significant.
func runRaw(dir string, args ...string) (string, error) {
	out, err := output(dir, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func run(dir string, args ...string) (string, error) {
	out, err := output(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// output runs git under a deadline. The context must outlive the call to
// Output, which is why it lives here rather than in a helper that only builds
// the command: cancelling it early would kill the process before it ran, and
// WaitDelay alone does not bound how long a hung git may take — it only limits
// I/O cleanup once the process has exited or the context has been cancelled.
// A hook runs inside a live Claude session, so an unbounded git is not an option.
func output(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, gitBinary, args...)
	cmd.Dir = dir
	// Once the deadline kills the process, do not wait indefinitely for pipes.
	cmd.WaitDelay = 2 * time.Second
	// Never let git stop for credentials or an editor inside a hook.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	return cmd.Output()
}
