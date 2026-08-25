package restore

import (
	"fmt"
	"regexp"
	"strings"
)

// Values recorded from a repository are not inherently trustworthy. A remote
// URL, branch name or commit is whatever was in the .git/config of a directory
// you happened to open — a shared checkout, an inherited configuration, a
// tarball that shipped its own .git. Git parses an argument beginning with "-"
// as an option, so a remote of "--upload-pack=<command>" executes that command
// during a clone. Every such value is therefore checked before it reaches git,
// and every git call also uses "--" so a positional can never be read as a flag.

var (
	shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	// Git refname rules are broader than this, but anything outside it is not
	// something ccvault needs to restore, and rejecting is safer than guessing.
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,255}$`)
)

// validateGitInputs refuses captured values that git could misinterpret. It
// runs during Prepare, so a dangerous record is reported before anything is
// written rather than part-way through a restore.
func validateGitInputs(remote, branch, sha string) error {
	if err := checkArg("remote URL", remote); err != nil {
		return err
	}
	if remote != "" && !plausibleRemote(remote) {
		return fmt.Errorf("refusing to clone %q: it does not look like a git URL or path", remote)
	}
	if branch != "" {
		if err := checkArg("branch name", branch); err != nil {
			return err
		}
		if branch != "HEAD" && !branchPattern.MatchString(branch) {
			return fmt.Errorf("refusing to check out %q: not a plausible branch name", branch)
		}
	}
	if sha != "" && !shaPattern.MatchString(sha) {
		return fmt.Errorf("refusing to check out %q: not a commit hash", sha)
	}
	return nil
}

// checkArg rejects anything git would read as an option.
func checkArg(what, v string) error {
	if strings.HasPrefix(v, "-") {
		return fmt.Errorf("refusing to use a %s beginning with \"-\" (%q): git would read it as a command-line option", what, v)
	}
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("refusing to use a %s containing a newline or null byte", what)
	}
	return nil
}

// plausibleRemote accepts the forms git actually clones from and nothing else.
func plausibleRemote(v string) bool {
	switch {
	case strings.HasPrefix(v, "/"), strings.HasPrefix(v, "./"), strings.HasPrefix(v, "../"):
		return true // local path
	case strings.HasPrefix(v, "~"):
		return true
	}
	for _, scheme := range []string{"https://", "http://", "git://", "ssh://", "file://", "git+ssh://"} {
		if strings.HasPrefix(v, scheme) {
			return true
		}
	}
	// scp-style: user@host:path
	if i := strings.Index(v, ":"); i > 0 && !strings.Contains(v[:i], "/") {
		return true
	}
	return false
}
