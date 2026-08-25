package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
	"github.com/sysn3rd/ccvault/internal/ingest"
	"github.com/sysn3rd/ccvault/internal/pending"
	"github.com/sysn3rd/ccvault/internal/snapshot"
)

// vaultUnavailable is returned instead of quietly creating a vault somewhere
// unexpected. Creating one inside an unmounted mountpoint is the specific
// disaster this guards against: the real vault reappears on remount, underneath
// a decoy, and neither is complete.
type vaultUnavailable struct {
	status config.Status
	cfg    *config.Config
}

func (e *vaultUnavailable) Error() string { return e.status.Detail }

// explain prints the situation and the ways out of it.
func (e *vaultUnavailable) explain(w io.Writer) {
	fmt.Fprintf(w, "\nThe vault is not available.\n\n  %s\n", e.status.Detail)
	if !e.status.LastSeen.IsZero() {
		fmt.Fprintf(w, "  last seen %s\n", relTime(e.status.LastSeen))
	}

	pendingNote(w)

	fmt.Fprintln(w, "\nWhat you can do:")
	switch e.status.State {
	case config.VaultUnmounted:
		fmt.Fprintln(w, "  · reconnect the drive, then run the command again")
		fmt.Fprintf(w, "  · point ccvault somewhere else:  ccvault config set vault_dir <path>\n")
	case config.VaultDeleted:
		fmt.Fprintln(w, "  · recreate an empty store here (no history):  ccvault vault init")
		fmt.Fprintf(w, "  · point ccvault at where the vault actually is: ccvault config set vault_dir <path>\n")
	case config.VaultOccupied:
		fmt.Fprintf(w, "  · choose an empty location:  ccvault config set vault_dir <path>\n")
		fmt.Fprintln(w, "  · or clear that directory yourself, then: ccvault vault init")
	default:
		fmt.Fprintln(w, "  · create it:  ccvault vault init")
	}
	fmt.Fprintf(w, "\nNothing is written until you choose. Settings: %s\n", e.cfg.Path)
}

// pendingNote reports captures held locally, so an unavailable vault never
// looks like lost work.
func pendingNote(w io.Writer) {
	dir, err := config.PendingDir()
	if err != nil {
		return
	}
	store, err := pending.Open(dir)
	if err != nil {
		return
	}
	n := store.Sessions()
	if n == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %d session(s) were captured while the vault was away, held at\n  %s (%s)\n",
		n, dir, humanBytes(store.Bytes()))
	fmt.Fprintln(w, "  They are safe. `ccvault pending adopt` files them once the vault is back.")
}

func runConfig(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return showConfig(cfg)
	}

	switch args[0] {
	case "path":
		fmt.Println(cfg.Path)
		return nil
	case "init":
		if cfg.Snapshots.IgnoreDirs == nil {
			cfg.Snapshots.IgnoreDirs = snapshot.DefaultIgnoreDirs
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", cfg.Path)
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: ccvault config set <key> <value>")
		}
		return setConfig(cfg, args[1], strings.Join(args[2:], " "))
	default:
		return fmt.Errorf("unknown config command %q (try: show, path, init, set)", args[0])
	}
}

func showConfig(cfg *config.Config) error {
	source := "defaults (no settings file yet)"
	if cfg.Loaded {
		source = cfg.Path
	}
	fmt.Printf("settings      %s\n", source)
	fmt.Printf("vault_dir     %s\n", cfg.VaultDir)
	fmt.Printf("claude_home   %s\n", cfg.ClaudeHome)
	fmt.Printf("\n[snapshots]\n")
	fmt.Printf("max_file_mb   %d\n", cfg.Snapshots.MaxFileMB)
	fmt.Printf("max_total_mb  %d\n", cfg.Snapshots.MaxTotalMB)
	ignore := cfg.Snapshots.IgnoreDirs
	if ignore == nil {
		ignore = snapshot.DefaultIgnoreDirs
	}
	fmt.Printf("ignore_dirs   %s\n", strings.Join(ignore, " "))
	fmt.Printf("\n[retention]\n")
	fmt.Printf("snapshots_kept   %d\n", cfg.Retention.Snapshots)
	fmt.Printf("git_states_kept  %d\n", cfg.Retention.GitStates)
	if cfg.Picker.Terminal != "" {
		fmt.Printf("\n[picker]\nterminal      %s\n", cfg.Picker.Terminal)
	}

	if len(cfg.FromEnv) > 0 {
		// Worth flagging loudly: an environment override reaches the hook (it
		// inherits your shell) but not the timer (it has none), which is
		// precisely how a vault ends up split in two.
		fmt.Printf("\n! overridden by environment: %s\n", strings.Join(cfg.FromEnv, ", "))
		fmt.Println("  Environment overrides are for testing. They do not reach the")
		fmt.Println("  reconcile timer, so captures would land in two different places.")
	}

	st := cfg.Check()
	fmt.Printf("\nvault status  %s\n", st.State)
	if st.State != config.VaultOK {
		fmt.Printf("              %s\n", st.Detail)
	}
	return nil
}

func setConfig(cfg *config.Config, key, value string) error {
	switch key {
	case "vault_dir":
		abs, err := filepath.Abs(expandHome(value))
		if err != nil {
			return err
		}
		cfg.VaultDir = abs
	case "claude_home":
		abs, err := filepath.Abs(expandHome(value))
		if err != nil {
			return err
		}
		cfg.ClaudeHome = abs
	case "max_file_mb", "max_total_mb", "snapshots_kept", "git_states_kept":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s needs a positive number, got %q", key, value)
		}
		switch key {
		case "max_file_mb":
			cfg.Snapshots.MaxFileMB = n
		case "max_total_mb":
			cfg.Snapshots.MaxTotalMB = n
		case "snapshots_kept":
			cfg.Retention.Snapshots = n
		case "git_states_kept":
			cfg.Retention.GitStates = n
		}
	case "ignore_dirs":
		cfg.Snapshots.IgnoreDirs = strings.Fields(value)
	case "terminal":
		cfg.Picker.Terminal = value
	default:
		return fmt.Errorf("unknown setting %q", key)
	}

	if cfg.Snapshots.IgnoreDirs == nil {
		cfg.Snapshots.IgnoreDirs = snapshot.DefaultIgnoreDirs
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("%s = %s\n", key, value)
	fmt.Printf("saved to %s\n", cfg.Path)

	if key == "vault_dir" {
		st := cfg.Check()
		if st.State != config.VaultOK {
			fmt.Printf("\nnote: %s\n", st.Detail)
			fmt.Println("      run `ccvault vault init` to create the store there,")
			fmt.Println("      or `ccvault vault move` to carry the existing one across.")
		}
	}
	return nil
}

func runVault(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccvault vault <init|move|status>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	switch args[0] {
	case "status":
		st := cfg.Check()
		fmt.Printf("%s\n%s\n", st.Path, st.State)
		if st.Detail != "" {
			fmt.Printf("%s\n", st.Detail)
		}
		pendingNote(os.Stdout)
		return nil

	case "init":
		st := cfg.Check()
		if st.State == config.VaultOK {
			fmt.Printf("vault already present at %s\n", cfg.VaultDir)
			return nil
		}
		if st.State == config.VaultOccupied {
			return fmt.Errorf("%s", st.Detail)
		}
		if st.State == config.VaultUnmounted {
			// Creating a decoy inside an unmounted mountpoint is the failure
			// this whole check exists to prevent, so it takes an explicit override.
			if len(args) < 2 || args[1] != "--anyway" {
				fmt.Printf("%s\n", st.Detail)
				fmt.Println("Reconnecting the drive is almost certainly what you want.")
				fmt.Println("To create a new empty store there regardless: ccvault vault init --anyway")
				return nil
			}
		}
		if err := cfg.EnsureDirs(); err != nil {
			return err
		}
		if err := cfg.RememberVault(); err != nil {
			return err
		}
		fmt.Printf("created an empty vault at %s\n", cfg.VaultDir)
		fmt.Println("run `ccvault scan` to index the sessions Claude Code still has on disk")
		return nil

	case "move":
		if len(args) < 2 {
			return fmt.Errorf("usage: ccvault vault move <new-path>")
		}
		return moveVault(cfg, args[1])

	default:
		return fmt.Errorf("unknown vault command %q (try: status, init, move)", args[0])
	}
}

// moveVault copies the vault to a new location, verifies it, then switches the
// settings over. The original is left in place: deleting the only copy of
// something irreplaceable is the user's call, not ours.
func moveVault(cfg *config.Config, dest string) error {
	st := cfg.Check()
	if st.State != config.VaultOK {
		return fmt.Errorf("the current vault is not available (%s), so there is nothing to move", st.State)
	}
	abs, err := filepath.Abs(expandHome(dest))
	if err != nil {
		return err
	}
	if abs == cfg.VaultDir {
		return fmt.Errorf("%s is already the vault location", abs)
	}

	if entries, err := os.ReadDir(abs); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty; choose an empty directory", abs)
	}

	fmt.Printf("moving vault\n  from %s\n  to   %s\n", cfg.VaultDir, abs)
	copied, bytes, err := copyTree(cfg.VaultDir, abs)
	if err != nil {
		return fmt.Errorf("copy failed, original left untouched: %w", err)
	}
	fmt.Printf("copied %d file(s), %s\n", copied, humanBytes(bytes))

	old := cfg.VaultDir
	cfg.VaultDir = abs
	if err := cfg.EnsureDirs(); err != nil {
		return err
	}
	// Prove the copy is usable before committing the settings to it.
	db, err := index.Open(cfg.DBPath())
	if err != nil {
		cfg.VaultDir = old
		return fmt.Errorf("the copied vault does not open, settings unchanged: %w", err)
	}
	sessions, _, _, statErr := statsTriple(db)
	db.Close()
	if statErr != nil {
		cfg.VaultDir = old
		return fmt.Errorf("the copied index is unreadable, settings unchanged: %w", statErr)
	}

	if err := cfg.Save(); err != nil {
		return err
	}
	if err := cfg.RememberVault(); err != nil {
		return err
	}
	fmt.Printf("\nvault is now %s (%d sessions)\n", abs, sessions)
	fmt.Printf("settings updated: %s\n", cfg.Path)
	fmt.Printf("\nThe original is still at %s — delete it once you are satisfied.\n", old)
	return nil
}

func statsTriple(db *index.DB) (int, int, int64, error) {
	st, err := db.Stats()
	if err != nil {
		return 0, 0, 0, err
	}
	return st.Sessions, st.Missing, st.TranscriptB, nil
}

func copyTree(src, dst string) (int, int64, error) {
	var files int
	var total int64
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		files++
		total += n
		return nil
	})
	return files, total, err
}

func runPending(args []string) error {
	dir, err := config.PendingDir()
	if err != nil {
		return err
	}
	store, err := pending.Open(dir)
	if err != nil {
		return err
	}
	records, err := store.List()
	if err != nil {
		return err
	}

	action := "list"
	if len(args) > 0 {
		action = args[0]
	}

	switch action {
	case "list":
		if len(records) == 0 {
			fmt.Println("nothing pending — every capture reached the vault")
			return nil
		}
		fmt.Printf("%d capture(s) held at %s (%s)\n\n", len(records), dir, humanBytes(store.Bytes()))
		for _, r := range records {
			fmt.Printf("%s  %-9s %s\n", r.SessionUUID[:8], r.Event, collapseHome(r.CWD))
			fmt.Printf("          captured %s", relTime(r.CapturedAt))
			if r.Git != nil && r.Git.HeadSHA != "" {
				fmt.Printf(" · %s@%s", orDash(r.Git.Branch), short(r.Git.HeadSHA))
			}
			fmt.Println()
		}
		fmt.Println("\n`ccvault pending adopt` files these into the vault.")
		return nil

	case "adopt":
		if len(records) == 0 {
			fmt.Println("nothing pending")
			return nil
		}
		cfg, db, err := open()
		if err != nil {
			return err
		}
		defer db.Close()
		res, err := ingest.Adopt(cfg, db)
		if err != nil {
			return err
		}
		for _, f := range res.Failed {
			fmt.Printf("  ! %s\n", f)
		}
		fmt.Printf("adopted %d capture(s) into %s\n", res.Adopted, cfg.VaultDir)
		return nil

	case "discard":
		if len(records) == 0 {
			fmt.Println("nothing pending")
			return nil
		}
		fmt.Printf("This permanently discards %d held capture(s).\n", len(records))
		fmt.Println("Commit SHAs and uncommitted diffs in them cannot be recovered afterwards.")
		fmt.Print("Proceed? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("cancelled")
			return nil
		}
		for _, r := range records {
			_ = store.Remove(r)
		}
		fmt.Printf("discarded %d capture(s)\n", len(records))
		return nil

	default:
		return fmt.Errorf("unknown pending command %q (try: list, adopt, discard)", action)
	}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
