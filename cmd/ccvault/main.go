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
	"github.com/sysn3rd/ccvault/internal/hooks"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/ingest"
	"github.com/sysn3rd/ccvault/internal/scheduler"
	"github.com/sysn3rd/ccvault/internal/tui"
)

const usage = `ccvault — inventory and backup for Claude Code context

  ccvault scan [--force]        Ingest new or grown transcripts; refresh directory state
  ccvault ls                    List every known session
  ccvault search [query]        Interactive picker; plain lines when piped
  ccvault show <uuid>           Everything known about one session
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
	fs.Parse(args)

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
	if res.Action == tui.ActionResume && res.Session != nil {
		return resume(res.Session)
	}
	return nil
}

// resume hands the terminal to Claude Code. Resuming is not reimplemented here:
// `claude --resume <uuid>` already works from any directory, so ccvault only has
// to put the process in the right one.
func resume(s *index.Session) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude not found on PATH: %w", err)
	}
	if err := os.Chdir(s.CWD); err != nil {
		return fmt.Errorf("cannot enter %s: %w", s.CWD, err)
	}
	// Replace this process outright; ccvault has nothing left to do.
	return syscall.Exec(bin, []string{"claude", "--resume", s.UUID}, os.Environ())
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

	s, err := db.Get(args[0])
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no session %s", args[0])
	}

	fmt.Printf("uuid        %s\n", s.UUID)
	fmt.Printf("title       %s\n", orDash(s.Title))
	fmt.Printf("cwd         %s  [%s]\n", s.CWD, s.DirState)
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
	if s.FirstPrompt != "" {
		fmt.Printf("\nfirst prompt\n  %s\n", s.FirstPrompt)
	}
	return nil
}

func runStatus(args []string) error {
	cfg, db, err := open()
	if err != nil {
		return err
	}
	defer db.Close()

	sessions, missing, bytes, err := db.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("vault        %s\n", cfg.VaultDir)
	fmt.Printf("claude home  %s\n", cfg.ClaudeHome)
	fmt.Printf("sessions     %d\n", sessions)
	fmt.Printf("missing dirs %d\n", missing)
	fmt.Printf("transcripts  %s\n", humanBytes(bytes))

	installed, err := hooksInstalled(cfg)
	if err != nil {
		return err
	}
	if installed {
		fmt.Println("hooks        installed")
	} else {
		fmt.Println("hooks        NOT installed — run: ccvault install-hooks")
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
	fs.Parse(args)

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
	fs.Parse(args)

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
		state := "OK"
		switch {
		case !s.HasTranscript():
			state = "NO TRANSCRIPT"
		case s.DirState == index.DirMissing:
			state = "MISSING"
		}
		fmt.Printf("%s  %-9s %-22s %s  [%s]\n",
			s.UUID[:8], relTime(s.LastActive), kind, collapseHome(s.CWD), state)
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
