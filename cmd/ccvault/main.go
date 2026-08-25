// Command ccvault indexes, backs up and restores Claude Code session context.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/gc"
	"github.com/sysn3rd/ccvault/internal/hooks"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/ingest"
	"github.com/sysn3rd/ccvault/internal/restore"
	"github.com/sysn3rd/ccvault/internal/scheduler"
	"github.com/sysn3rd/ccvault/internal/tui"
)

const usage = `ccvault — inventory and backup for Claude Code context

  ccvault scan [--force]        Ingest new or grown transcripts; refresh directory state
  ccvault ls                    List every known session
  ccvault search [query]        Interactive picker; plain lines when piped
  ccvault show <uuid>           Everything known about one session
  ccvault restore <uuid>        Rebuild the session's directory, then resume
                                  [--to DIR] [--force] [--no-resume] [--fork] [-n]
  ccvault forget <uuid> [--yes] Drop a session from the vault (does not touch ~/.claude)
  ccvault gc [-n] [--older-than DUR]  Reclaim space; never touches a live transcript
  ccvault status                Vault health
  ccvault install-hooks [-n]    Merge capture hooks into ~/.claude/settings.json
  ccvault install-timer [-n]    Install the periodic reconcile job (systemd / launchd)
  ccvault capture --hook        Internal: invoked by the hooks above (reads JSON on stdin)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// capture is special: it runs inside a live Claude session and must never
	// fail in a way the user notices.
	if cmd == "capture" {
		runCapture(args)
		return
	}

	var err error
	switch cmd {
	case "scan":
		err = runScan(args)
	case "ls":
		err = runList(args)
	case "search":
		err = runSearch(args)
	case "show":
		err = runShow(args)
	case "restore":
		err = runRestore(args)
	case "forget":
		err = runForget(args)
	case "gc":
		err = runGC(args)
	case "status":
		err = runStatus(args)
	case "install-hooks":
		err = runInstallHooks(args)
	case "install-timer":
		err = runInstallTimer(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccvault:", err)
		os.Exit(1)
	}
}

func open() (*config.Config, *index.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, nil, err
	}
	db, err := index.Open(cfg.DBPath())
	if err != nil {
		return nil, nil, err
	}
	return cfg, db, nil
}

// hookPayload is the JSON Claude Code writes to a hook's stdin.
type hookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Reason         string `json:"reason"`
}

// runCapture is invoked from inside a Claude session. It exits 0 no matter what:
// a backup tool must never be the reason a session stalls or errors.
func runCapture(args []string) {
	defer func() {
		recover()
		os.Exit(0)
	}()

	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hook := fs.Bool("hook", false, "read hook JSON from stdin")
	_ = fs.Parse(args)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }()
		captureOnce(*hook)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		// Give up silently rather than hold the session open.
	}
	os.Exit(0)
}

func captureOnce(fromHook bool) {
	var p hookPayload
	if fromHook {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil {
			return
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
	}
	if p.SessionID == "" {
		return
	}

	cfg, db, err := open()
	if err != nil {
		return
	}
	defer db.Close()

	path := p.TranscriptPath
	if path == "" {
		path = findTranscript(cfg, p.SessionID)
	}
	if path == "" {
		return
	}

	event := "scan"
	switch p.HookEventName {
	case "SessionStart":
		event = "start"
	case "SessionEnd":
		event = "end"
	}
	_, _ = ingest.Ingest(cfg, db, path, event, true)
}

// findTranscript locates a transcript by session id across every project shard.
// The shard name is a lossy slug of the cwd and cannot be computed reliably, so
// we search by filename instead.
func findTranscript(cfg *config.Config, uuid string) string {
	matches, err := filepath.Glob(filepath.Join(cfg.ProjectsDir(), "*", uuid+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func runScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)

	force := fs.Bool("force", false, "re-ingest every transcript, even unchanged ones")
	fs.Parse(permute(fs, args))

	cfg, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	res, err := ingest.ScanAll(cfg, db, *force)
	if err != nil {
		return err
	}
	fmt.Printf("scanned %d  updated %d  skipped %d  failed %d\n",
		res.Scanned, res.Updated, res.Skipped, res.Failed)
	if res.Seeded > 0 {
		fmt.Printf("recovered %d session(s) from the prompt log (transcripts already pruned)\n", res.Seeded)
	}
	return nil
}

func runList(args []string) error {
	_, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	sessions, err := db.List()
	if err != nil {
		return err
	}
	printTable(sessions)
	return nil
}

func runSearch(args []string) error {
	_, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	query := strings.Join(args, " ")

	// Piped or redirected: emit plain lines and pass the query to FTS verbatim,
	// so boolean syntax like `kali OR pentest` still works in scripts.
	if !isTerminal() {
		if query == "" {
			sessions, err := db.List()
			if err != nil {
				return err
			}
			printTable(sessions)
			return nil
		}
		sessions, err := db.Search(query)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Println("no matches")
			return nil
		}
		printTable(sessions)
		return nil
	}

	res, err := tui.Run(db, query)
	if err != nil {
		return err
	}
	switch res.Action {
	case tui.ActionResume:
		return resumeIn(res.Session, res.Session.CWD, false)
	case tui.ActionRestore:
		return runRestore([]string{res.Session.UUID})
	}
	return nil
}

// resumeIn hands the terminal to Claude Code. Resuming is not reimplemented
// here: `claude --resume <uuid>` already works from any directory, so ccvault
// only has to put the process in the right one.
func resumeIn(s *index.Session, dir string, fork bool) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found on PATH: %w", err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("cannot enter %s: %w", dir, err)
	}
	argv := []string{"claude", "--resume", s.UUID}
	if fork {
		// Plain resume appends to the original transcript; forking leaves the
		// archived one untouched.
		argv = append(argv, "--fork-session")
	}
	// Replace this process outright; ccvault has nothing left to do.
	return syscall.Exec(bin, argv, os.Environ())
}

func isTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func runShow(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("show needs a session uuid")
	}
	_, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	s, err := resolveSession(db, args[0])
	if err != nil {
		return err
	}

	fmt.Printf("uuid        %s\n", s.UUID)
	fmt.Printf("title       %s\n", orDash(s.Title))
	fmt.Printf("cwd         %s  [%s]\n", s.CWD, s.DirState)
	if !s.HasTranscript() {
		fmt.Printf("            (prompt log only — Claude Code pruned the transcript)\n")
	}
	if len(s.CWDs) > 1 {
		fmt.Printf("also ran in %s\n", strings.Join(s.CWDs[1:], "\n            "))
	}
	fmt.Printf("kind        %s\n", s.Kind)
	fmt.Printf("started     %s\n", s.StartedAt.Format(time.RFC3339))
	fmt.Printf("last active %s\n", s.LastActive.Format(time.RFC3339))
	fmt.Printf("turns       %d\n", s.MsgCount)
	fmt.Printf("transcript  %s (%s)\n", s.VaultPath, humanBytes(s.Bytes))
	fmt.Printf("cc version  %s\n", orDash(s.CCVersion))

	g, err := db.LatestGitState(s.UUID)
	if err == nil && g != nil {
		fmt.Printf("\ngit  remote %s\n", orDash(g.RemoteURL))
		fmt.Printf("     branch %s @ %s\n", orDash(g.Branch), short(g.HeadSHA))
		fmt.Printf("     dirty  %t\n", g.IsDirty)
		if g.PatchPath != "" {
			fmt.Printf("     patch  %s\n", g.PatchPath)
		}
		if g.UntrackedPath != "" {
			fmt.Printf("     untracked bundle %s\n", g.UntrackedPath)
		}
		if g.UntrackedSkipped != "" {
			fmt.Printf("     SKIPPED untracked: %s\n", g.UntrackedSkipped)
		}
		fmt.Printf("     .claude tracked in git: %t\n", g.ClaudeDirTracked)
		fmt.Printf("     captured at %s (%s)\n", g.CapturedAt.Format(time.RFC3339), g.Event)
	}
	for _, kind := range []string{index.SnapTree, index.SnapClaude} {
		snap, err := db.LatestSnapshot(s.UUID, kind)
		if err != nil || snap == nil {
			continue
		}
		fmt.Printf("\nsnapshot (%s)  %s\n", kind, snap.Path)
		fmt.Printf("     %d file(s), %s, taken %s\n",
			snap.FileCount, humanBytes(snap.Bytes), snap.CapturedAt.Format(time.RFC3339))
		if snap.SkippedJSON != "" {
			var skipped []string
			if json.Unmarshal([]byte(snap.SkippedJSON), &skipped) == nil && len(skipped) > 0 {
				fmt.Printf("     EXCLUDED %d path(s):\n", len(skipped))
				for i, sk := range skipped {
					if i >= 5 {
						fmt.Printf("       … and %d more\n", len(skipped)-i)
						break
					}
					fmt.Printf("       %s\n", sk)
				}
			}
		}
	}

	if s.FirstPrompt != "" {
		fmt.Printf("\nfirst prompt\n  %s\n", s.FirstPrompt)
	}
	return nil
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)

	to := fs.String("to", "", "rebuild somewhere other than the original directory")
	force := fs.Bool("force", false, "write into a directory that already has contents")
	dry := fs.Bool("n", false, "show the plan without writing anything")
	noResume := fs.Bool("no-resume", false, "rebuild the directory but do not launch Claude")
	fork := fs.Bool("fork", false, "resume into a new session id, leaving the original transcript untouched")
	fs.Parse(permute(fs, args))

	if fs.NArg() == 0 {
		return fmt.Errorf("restore needs a session uuid (see `ccvault ls`)")
	}

	_, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	s, err := resolveSession(db, fs.Arg(0))
	if err != nil {
		return err
	}

	plan, err := restore.Prepare(db, s, restore.Options{Target: *to, Force: *force})
	if err != nil {
		return err
	}

	fmt.Printf("restore %s  %s\n", s.UUID[:8], orDash(firstLine(s.Title, s.FirstPrompt)))
	fmt.Printf("  into     %s\n", collapseHome(plan.Target))
	fmt.Printf("  strategy %s\n", plan.Strategy)
	for _, step := range plan.Steps {
		fmt.Printf("    · %s\n", step)
	}
	for _, w := range plan.Warnings {
		fmt.Printf("  ! %s\n", w)
	}
	if *dry {
		fmt.Println("\n(dry run — nothing was written)")
		return nil
	}

	fmt.Println()
	report, err := restore.Execute(plan)
	if err != nil {
		return err
	}
	for _, c := range report.Completed {
		fmt.Printf("  ✓ %s\n", c)
	}
	for _, w := range report.Warnings {
		fmt.Printf("  ! %s\n", w)
	}

	if *noResume || !s.HasTranscript() {
		fmt.Printf("\nrestored to %s\n", collapseHome(plan.Target))
		if !s.HasTranscript() {
			fmt.Println("(no transcript to resume — the directory is rebuilt, the conversation is not)")
		}
		return nil
	}

	fmt.Printf("\nresuming in %s\n", collapseHome(plan.Target))
	return resumeIn(s, plan.Target, *fork)
}

// resolveSession accepts a full uuid or any unambiguous prefix, because nobody
// types a uuid by hand.
func resolveSession(db *index.DB, ref string) (*index.Session, error) {
	if s, err := db.Get(ref); err == nil && s != nil {
		return s, nil
	}
	all, err := db.List()
	if err != nil {
		return nil, err
	}
	var matches []*index.Session
	for _, s := range all {
		if strings.HasPrefix(s.UUID, ref) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no session matching %q", ref)
	case 1:
		return matches[0], nil
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, m.UUID[:8])
		}
		return nil, fmt.Errorf("%q matches %d sessions: %s", ref, len(matches), strings.Join(ids, ", "))
	}
}

func runForget(args []string) error {
	fs := flag.NewFlagSet("forget", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Parse(permute(fs, args))

	if fs.NArg() == 0 {
		return fmt.Errorf("forget needs a session uuid")
	}
	_, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	s, err := resolveSession(db, fs.Arg(0))
	if err != nil {
		return err
	}

	fmt.Printf("forget %s  %s\n", s.UUID[:8], orDash(firstLine(s.Title, s.FirstPrompt)))
	fmt.Printf("  %s\n", collapseHome(s.CWD))
	fmt.Println("  This deletes ccvault's copies. Claude Code's own files are left alone,")
	fmt.Println("  so a later scan will re-index the session unless you delete those too.")

	if !*yes {
		fmt.Print("\nProceed? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("cancelled")
			return nil
		}
	}

	files, err := db.Forget(s.UUID)
	if err != nil {
		return err
	}
	removed := 0
	for _, f := range files {
		if err := os.Remove(f); err == nil {
			removed++
		}
	}
	fmt.Printf("forgot %s (%d file(s) removed)\n", s.UUID[:8], removed)
	return nil
}

func runGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	dry := fs.Bool("n", false, "show what would be removed without removing it")
	olderThan := fs.Duration("older-than", 0, "also prune supporting files older than this (e.g. 720h)")
	fs.Parse(permute(fs, args))

	cfg, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	plan, err := gc.Compute(cfg, db, gc.Options{OlderThan: *olderThan})
	if err != nil {
		return err
	}
	if len(plan.Candidates) == 0 && len(plan.Dangling) == 0 {
		fmt.Println("nothing to collect")
		return nil
	}

	for _, c := range plan.Candidates {
		if c.Path == "" {
			continue
		}
		fmt.Printf("  %-9s %s\n             %s\n", humanBytes(c.Bytes), filepath.Base(c.Path), c.Reason)
	}
	if n := len(plan.Dangling); n > 0 {
		fmt.Printf("  %d index row(s) point at files that are already gone\n", n)
	}
	fmt.Printf("\nwould free %s\n", humanBytes(plan.FreedBytes))

	if *dry {
		fmt.Println("(dry run — nothing was removed)")
		return nil
	}

	report, err := gc.Execute(db, plan)
	if err != nil {
		return err
	}
	for _, f := range report.Failed {
		fmt.Printf("  ! %s\n", f)
	}
	if err := db.Vacuum(); err != nil {
		fmt.Printf("  ! could not compact the index: %v\n", err)
	}
	fmt.Printf("removed %d file(s), freed %s\n", report.Removed, humanBytes(report.FreedBytes))
	return nil
}

func runStatus(args []string) error {
	cfg, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := db.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("vault         %s\n", cfg.VaultDir)
	fmt.Printf("claude home   %s\n", cfg.ClaudeHome)
	fmt.Printf("sessions      %d\n", st.Sessions)
	fmt.Printf("  deleted dir %d\n", st.Missing)
	fmt.Printf("  empty dir   %d\n", st.Empty)
	fmt.Printf("  pruned      %d  (prompt log only)\n", st.NoTranscript)
	fmt.Printf("transcripts   %s\n", humanBytes(st.TranscriptB))
	fmt.Printf("snapshots     %s in %d file(s)\n", humanBytes(st.SnapshotB), st.SnapshotFiles)

	installed, err := hooksInstalled(cfg)
	if err != nil {
		return err
	}
	if installed {
		fmt.Println("hooks         installed")
	} else {
		fmt.Println("hooks         NOT installed — run: ccvault install-hooks")
	}
	return nil
}

func hooksInstalled(cfg *config.Config) (bool, error) {
	plan, err := hooks.Install(cfg.SettingsFile(), currentBinary(), true)
	if err != nil {
		return false, err
	}
	return len(plan.Added) == 0, nil
}

func runInstallHooks(args []string) error {
	fs := flag.NewFlagSet("install-hooks", flag.ExitOnError)

	dry := fs.Bool("n", false, "show what would change without writing")
	fs.Parse(permute(fs, args))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	plan, err := hooks.Install(cfg.SettingsFile(), currentBinary(), *dry)
	if err != nil {
		return err
	}

	fmt.Printf("settings %s\n", plan.SettingsPath)
	for _, e := range plan.AlreadyOK {
		fmt.Printf("  %-13s already installed\n", e)
	}
	for _, e := range plan.Added {
		verb := "added"
		if *dry {
			verb = "would add"
		}
		fmt.Printf("  %-13s %s\n", e, verb)
	}
	if plan.BackupPath != "" {
		fmt.Printf("backup   %s\n", plan.BackupPath)
	}
	if len(plan.Added) > 0 && !*dry {
		fmt.Println("\nHooks apply to sessions started from now on.")
	}
	return nil
}

func runInstallTimer(args []string) error {
	fs := flag.NewFlagSet("install-timer", flag.ExitOnError)

	dry := fs.Bool("n", false, "show what would be written without writing")
	fs.Parse(permute(fs, args))

	plan, err := scheduler.Install(currentBinary(), *dry)
	if err != nil {
		return err
	}
	for _, f := range plan.Files {
		verb := "wrote"
		if *dry {
			verb = "would write"
		}
		fmt.Printf("%s %s\n", verb, f)
	}
	switch {
	case *dry:
	case plan.Enabled:
		fmt.Printf("timer active — reconcile runs every %dm\n", scheduler.IntervalSeconds/60)
	case plan.Hint != "":
		fmt.Println(plan.Hint)
	}
	return nil
}

// permute moves flags ahead of positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `ccvault restore <uuid> -n` would silently treat -n as a positional and run a
// real restore when a preview was asked for. Flags that take a value have that
// value moved with them.
func permute(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue // value supplied inline
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	// The explicit terminator makes flag.Parse stop at the same boundary we
	// computed, so a positional that looks like a flag stays positional.
	return append(append(flags, "--"), positional...)
}

func currentBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "ccvault"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

func printTable(sessions []*index.Session) {
	for _, s := range sessions {
		kind := s.Kind
		if ref := s.GitRef(); ref != "" {
			kind = s.Kind + " " + ref
		}
		fmt.Printf("%s  %-9s %-22s %s  [%s]\n",
			s.UUID[:8], relTime(s.LastActive), kind, collapseHome(s.CWD), s.StateLabel())
		fmt.Printf("          %s\n", orDash(firstLine(s.Title, s.FirstPrompt)))
	}
}

func firstLine(title, fallback string) string {
	v := title
	if v == "" {
		v = fallback
	}
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	if len(v) > 78 {
		v = v[:78] + "…"
	}
	return v
}

func collapseHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(p, home) {
		return p
	}
	return "~" + strings.TrimPrefix(p, home)
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGT"[exp])
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return orDash(sha)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
