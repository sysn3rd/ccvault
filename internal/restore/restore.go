// Package restore rebuilds the working directory a session ran in.
//
// Two strategies, chosen by what was captured. A repository with a remote is
// re-cloned at the exact commit and its uncommitted work replayed on top — cheap
// and faithful. Anything else is unpacked from a tree archive.
package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/snapshot"
)

type Strategy string

const (
	StrategyClone    Strategy = "clone"    // git clone + checkout + replay
	StrategySnapshot Strategy = "snapshot" // unpack a tree archive
)

type Options struct {
	// Target is where to rebuild. Empty means the session's original directory,
	// which is preferred: Claude Code's own /resume picker then works normally.
	Target string
	// Force permits writing into a directory that already has contents.
	Force bool
}

// Plan is what a restore would do. It is computed before anything is written so
// the caller can show it and stop.
type Plan struct {
	Session  *index.Session
	Target   string
	Strategy Strategy

	RemoteURL string
	Branch    string
	HeadSHA   string

	PatchPath     string
	UntrackedPath string
	SnapshotPath  string
	ClaudePath    string

	Steps    []string
	Warnings []string
}

// Report records what actually happened.
type Report struct {
	Plan      *Plan
	Completed []string
	Warnings  []string
}

const gitTimeout = 5 * time.Minute

// Prepare works out how to rebuild a session's directory, and refuses up front
// rather than half-way through.
func Prepare(db *index.DB, s *index.Session, opts Options) (*Plan, error) {
	p := &Plan{Session: s, Target: opts.Target}
	if p.Target == "" {
		p.Target = s.CWD
	}
	if p.Target == "" {
		return nil, fmt.Errorf("session %s has no recorded directory", short(s.UUID))
	}
	abs, err := filepath.Abs(p.Target)
	if err != nil {
		return nil, err
	}
	p.Target = abs

	if err := checkTarget(p.Target, opts.Force); err != nil {
		return nil, err
	}

	gs, _ := db.LatestGitState(s.UUID)
	tree, _ := db.LatestSnapshot(s.UUID, index.SnapTree)

	switch {
	case gs != nil && gs.RemoteURL != "":
		p.Strategy = StrategyClone
		p.RemoteURL = gs.RemoteURL
		p.Branch = gs.Branch
		p.HeadSHA = gs.HeadSHA
		p.PatchPath = existingFile(gs.PatchPath)
		p.UntrackedPath = existingFile(gs.UntrackedPath)
		if gs.PatchPath != "" && p.PatchPath == "" {
			p.Warnings = append(p.Warnings, "the recorded dirty patch is missing from the vault; uncommitted changes will not be replayed")
		}
		if claude, _ := db.LatestSnapshot(s.UUID, index.SnapClaude); claude != nil {
			p.ClaudePath = existingFile(claude.Path)
		}
	case tree != nil:
		p.Strategy = StrategySnapshot
		p.SnapshotPath = existingFile(tree.Path)
		if p.SnapshotPath == "" {
			return nil, fmt.Errorf("the archive for %s is recorded but missing from the vault", short(s.UUID))
		}
		if tree.SkippedJSON != "" {
			var skipped []string
			if json.Unmarshal([]byte(tree.SkippedJSON), &skipped) == nil && len(skipped) > 0 {
				p.Warnings = append(p.Warnings, fmt.Sprintf(
					"%d path(s) were excluded when this was archived and will not come back (see `ccvault show %s`)",
					len(skipped), short(s.UUID)))
			}
		}
	default:
		return nil, fmt.Errorf(
			"nothing to restore from: %s has no remote and no archive.\n"+
				"Only directories ccvault saw while they still existed can be rebuilt.", short(s.UUID))
	}

	if !s.HasTranscript() {
		p.Warnings = append(p.Warnings,
			"Claude Code already pruned this transcript, so the directory can be rebuilt but the conversation cannot be resumed")
	}
	p.Steps = describe(p)
	return p, nil
}

func describe(p *Plan) []string {
	var steps []string
	switch p.Strategy {
	case StrategyClone:
		steps = append(steps, "clone "+p.RemoteURL)
		if p.HeadSHA != "" {
			ref := shortSHA(p.HeadSHA)
			if p.Branch != "" && p.Branch != "HEAD" {
				steps = append(steps, fmt.Sprintf("check out %s at %s", p.Branch, ref))
			} else {
				steps = append(steps, "check out "+ref)
			}
		}
		if p.PatchPath != "" {
			steps = append(steps, "replay uncommitted changes")
		}
		if p.UntrackedPath != "" {
			steps = append(steps, "restore untracked files")
		}
		if p.ClaudePath != "" {
			steps = append(steps, "restore .claude/ (not tracked by the repo)")
		}
	case StrategySnapshot:
		steps = append(steps, "unpack archive")
	}
	return steps
}

// checkTarget refuses to write over work that is already there. An empty
// directory is fine: that is exactly the ~/code/games case.
func checkTarget(target string, force bool) error {
	info, err := os.Stat(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("%s already has %d entries; refusing to write into it (use --force to override)",
			target, len(entries))
	}
	return nil
}

// Execute performs the plan. It never deletes anything the user already had.
func Execute(p *Plan) (*Report, error) {
	r := &Report{Plan: p, Warnings: append([]string{}, p.Warnings...)}

	switch p.Strategy {
	case StrategyClone:
		if err := executeClone(p, r); err != nil {
			return r, err
		}
	case StrategySnapshot:
		if err := os.MkdirAll(p.Target, 0o755); err != nil {
			return r, err
		}
		if err := snapshot.Extract(p.SnapshotPath, p.Target); err != nil {
			return r, fmt.Errorf("unpacking archive: %w", err)
		}
		r.Completed = append(r.Completed, "unpacked archive")
	}
	return r, nil
}

func executeClone(p *Plan, r *Report) error {
	if err := os.MkdirAll(filepath.Dir(p.Target), 0o755); err != nil {
		return err
	}
	if _, err := git(".", "clone", p.RemoteURL, p.Target); err != nil {
		return fmt.Errorf("cloning %s: %w", p.RemoteURL, err)
	}
	r.Completed = append(r.Completed, "cloned "+p.RemoteURL)

	if p.HeadSHA != "" {
		// The recorded commit may never have been pushed, in which case the
		// clone simply does not contain it. Say so plainly and leave the clone
		// on its default branch rather than failing with a raw git error.
		if _, err := git(p.Target, "cat-file", "-e", p.HeadSHA+"^{commit}"); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf(
				"commit %s is not on the remote (it was never pushed); the clone is on its default branch instead",
				shortSHA(p.HeadSHA)))
			return nil
		}
		branch := p.Branch
		if branch == "" || branch == "HEAD" {
			if _, err := git(p.Target, "checkout", "--detach", p.HeadSHA); err != nil {
				return fmt.Errorf("checking out %s: %w", shortSHA(p.HeadSHA), err)
			}
			r.Completed = append(r.Completed, "checked out "+shortSHA(p.HeadSHA))
		} else {
			if _, err := git(p.Target, "checkout", "-B", branch, p.HeadSHA); err != nil {
				return fmt.Errorf("checking out %s at %s: %w", branch, shortSHA(p.HeadSHA), err)
			}
			r.Completed = append(r.Completed, fmt.Sprintf("checked out %s at %s", branch, shortSHA(p.HeadSHA)))
		}
	}

	if p.PatchPath != "" {
		if _, err := git(p.Target, "apply", "--3way", p.PatchPath); err != nil {
			// A patch that will not apply is worth reporting, not aborting over:
			// the clone at the right commit is already most of the value.
			r.Warnings = append(r.Warnings,
				"the uncommitted changes did not apply cleanly; the patch is at "+p.PatchPath)
		} else {
			r.Completed = append(r.Completed, "replayed uncommitted changes")
		}
	}

	if p.UntrackedPath != "" {
		if err := snapshot.Extract(p.UntrackedPath, p.Target); err != nil {
			r.Warnings = append(r.Warnings, "could not restore untracked files: "+err.Error())
		} else {
			r.Completed = append(r.Completed, "restored untracked files")
		}
	}

	if p.ClaudePath != "" {
		dest := filepath.Join(p.Target, ".claude")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			r.Warnings = append(r.Warnings, "could not restore .claude/: "+err.Error())
		} else if err := snapshot.Extract(p.ClaudePath, dest); err != nil {
			r.Warnings = append(r.Warnings, "could not restore .claude/: "+err.Error())
		} else {
			r.Completed = append(r.Completed, "restored .claude/")
		}
	}
	return nil
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "." {
		cmd.Dir = dir
	}
	// Never let a restore hang waiting for credentials at a prompt.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func existingFile(p string) string {
	if p == "" {
		return ""
	}
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func short(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8]
	}
	return uuid
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
