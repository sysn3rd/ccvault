package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the built binary rather than calling functions directly.
//
// Every previous bug in the command layer was in the seam between the user and
// the code — a flag parsed as a positional, a dry run that wrote to disk — and
// none of them were reachable by testing internals. Running the real binary with
// real arguments is the only way those show up.

var (
	binaryPath string
	// coverDir collects coverage from the binary under test. Subprocess
	// coverage is not attributed automatically: without building with -cover
	// and pointing GOCOVERDIR at a directory, every command handler reads as
	// untested even when these tests exercise all of them.
	coverDir string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ccvault-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binaryPath = filepath.Join(dir, "ccvault")
	args := []string{"build", "-o", binaryPath}
	if testing.CoverMode() != "" {
		// CCVAULT_COVERDIR lets CI collect the raw profiles; otherwise they go
		// somewhere temporary and are simply discarded.
		coverDir = os.Getenv("CCVAULT_COVERDIR")
		if coverDir == "" {
			coverDir = filepath.Join(dir, "covdata")
		}
		if err := os.MkdirAll(coverDir, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create coverage dir:", err)
			os.Exit(1)
		}
		args = append(args, "-cover", "-coverpkg=github.com/sysn3rd/ccvault/...")
	}
	args = append(args, ".")

	build := exec.Command("go", args...)
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// env is an isolated home: its own settings, state, vault and Claude directory,
// so nothing touches the machine running the tests.
type env struct {
	root       string
	claudeHome string
	vaultDir   string
	t          *testing.T
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		root:       root,
		claudeHome: filepath.Join(root, "claude"),
		vaultDir:   filepath.Join(root, "data", "ccvault"),
		t:          t,
	}
	for _, d := range []string{
		e.claudeHome,
		filepath.Join(e.claudeHome, "projects"),
		filepath.Join(root, "config"),
		filepath.Join(root, "state"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) vars() []string {
	vars := os.Environ()
	if coverDir != "" {
		vars = append(vars, "GOCOVERDIR="+coverDir)
	}
	return append(vars,
		"HOME="+e.root,
		"XDG_CONFIG_HOME="+filepath.Join(e.root, "config"),
		"XDG_STATE_HOME="+filepath.Join(e.root, "state"),
		"XDG_DATA_HOME="+filepath.Join(e.root, "data"),
		"CCVAULT_CLAUDE_HOME="+e.claudeHome,
		// Keep the picker out of it; these tests drive the non-interactive path.
		"TERM=dumb",
	)
}

type result struct {
	stdout, stderr string
	code           int
}

func (r result) out() string { return r.stdout + r.stderr }

// run invokes the binary with stdout captured, which also exercises the
// "piped output stays plain" branch.
func (e *env) run(args ...string) result {
	e.t.Helper()
	cmd := exec.Command(binaryPath, args...)
	cmd.Env = e.vars()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if err != nil {
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			e.t.Fatalf("running %v: %v", args, err)
		}
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// addSession writes a transcript into the fake Claude home.
func (e *env) addSession(slug, uuid, cwd, prompt string) {
	e.t.Helper()
	dir := filepath.Join(e.claudeHome, "projects", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	rec := map[string]any{
		"type": "user", "sessionId": uuid, "cwd": cwd,
		"version": "2.1.240", "gitBranch": "main",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message":   map[string]any{"role": "user", "content": prompt},
	}
	line, err := json.Marshal(rec)
	if err != nil {
		e.t.Fatal(err)
	}
	path := filepath.Join(dir, uuid+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// liveDir is a directory with something in it, so it is not reported EMPTY.
func (e *env) liveDir(name string) string {
	e.t.Helper()
	d := filepath.Join(e.root, "work", name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "file.txt"), []byte("x"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestVersionAndHelp(t *testing.T) {
	e := newEnv(t)

	v := e.run("version")
	if v.code != 0 || !strings.HasPrefix(v.stdout, "ccvault ") {
		t.Errorf("version = %q (exit %d)", v.stdout, v.code)
	}
	for _, flag := range []string{"--version", "-v"} {
		if got := e.run(flag); !strings.HasPrefix(got.stdout, "ccvault ") {
			t.Errorf("%s = %q", flag, got.stdout)
		}
	}

	h := e.run("--help")
	for _, want := range []string{"ccvault search", "ccvault restore", "ccvault config"} {
		if !strings.Contains(h.stdout, want) {
			t.Errorf("help does not mention %q", want)
		}
	}

	// An unknown command must fail loudly rather than doing something.
	bad := e.run("definitely-not-a-command")
	if bad.code == 0 {
		t.Error("an unknown command exited 0")
	}
	if !strings.Contains(bad.out(), "unknown command") {
		t.Errorf("unhelpful error: %q", bad.out())
	}
}

func TestScanListAndShow(t *testing.T) {
	e := newEnv(t)
	live := e.liveDir("project")
	const uuid = "11111111-2222-3333-4444-555555555555"
	e.addSession("-project", uuid, live, "how do I fix the flaky test")

	scan := e.run("scan")
	if scan.code != 0 {
		t.Fatalf("scan failed: %s", scan.out())
	}
	if !strings.Contains(scan.stdout, "updated 1") {
		t.Errorf("scan output = %q", scan.stdout)
	}

	ls := e.run("ls")
	if !strings.Contains(ls.stdout, uuid[:8]) || !strings.Contains(ls.stdout, "flaky test") {
		t.Errorf("ls output = %q", ls.stdout)
	}
	if !strings.Contains(ls.stdout, "[OK]") {
		t.Errorf("a live directory should be OK: %q", ls.stdout)
	}

	// A uuid prefix must be enough; nobody types a full one.
	show := e.run("show", uuid[:8])
	if show.code != 0 {
		t.Fatalf("show by prefix failed: %s", show.out())
	}
	for _, want := range []string{uuid, live, "first prompt"} {
		if !strings.Contains(show.stdout, want) {
			t.Errorf("show output missing %q:\n%s", want, show.stdout)
		}
	}

	missing := e.run("show", "does-not-exist")
	if missing.code == 0 {
		t.Error("show of an unknown session exited 0")
	}
}

// Search must stay plain and pass the query through when it is not a terminal,
// so it composes in scripts.
func TestSearchPipedIsPlain(t *testing.T) {
	e := newEnv(t)
	live := e.liveDir("p")
	e.addSession("-p", "aaaaaaaa-0000-0000-0000-000000000000", live, "investigate the oauth redirect")
	e.addSession("-p", "bbbbbbbb-0000-0000-0000-000000000000", live, "rewrite the billing importer")
	e.run("scan")

	hit := e.run("search", "oauth")
	if !strings.Contains(hit.stdout, "aaaaaaaa") {
		t.Errorf("search missed the match: %q", hit.stdout)
	}
	if strings.Contains(hit.stdout, "bbbbbbbb") {
		t.Errorf("search returned an unrelated session: %q", hit.stdout)
	}

	// Boolean FTS syntax has to survive being passed through.
	both := e.run("search", "oauth OR billing")
	for _, want := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if !strings.Contains(both.stdout, want) {
			t.Errorf("boolean search missed %s: %q", want, both.stdout)
		}
	}

	none := e.run("search", "zzzznotathing")
	if !strings.Contains(none.stdout, "no matches") {
		t.Errorf("expected 'no matches', got %q", none.stdout)
	}
}

// The bug this pins: `restore <id> -n` used to perform a real restore, because
// Go's flag package stops parsing at the first positional argument.
func TestRestoreDryRunWritesNothing(t *testing.T) {
	e := newEnv(t)
	work := e.liveDir("snapshot-me")
	const uuid = "cccccccc-0000-0000-0000-000000000000"
	e.addSession("-s", uuid, work, "a plain directory session")
	e.run("scan")

	// Remove the directory so a restore has something to do.
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	e.run("scan")

	if ls := e.run("ls"); !strings.Contains(ls.stdout, "[MISSING]") {
		t.Fatalf("directory should be MISSING: %q", ls.stdout)
	}

	dry := e.run("restore", uuid[:8], "-n")
	if dry.code != 0 {
		t.Fatalf("dry run failed: %s", dry.out())
	}
	if !strings.Contains(dry.stdout, "dry run") {
		t.Errorf("output does not say it was a dry run: %q", dry.stdout)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote to disk")
	}

	// And the real thing does restore it.
	real := e.run("restore", uuid[:8], "--no-resume")
	if real.code != 0 {
		t.Fatalf("restore failed: %s", real.out())
	}
	if _, err := os.Stat(filepath.Join(work, "file.txt")); err != nil {
		t.Errorf("restore did not rebuild the directory: %v", err)
	}
}

func TestRestoreExplainsWhenNothingWasCaptured(t *testing.T) {
	e := newEnv(t)
	const uuid = "dddddddd-0000-0000-0000-000000000000"
	// A directory that never existed while ccvault was watching.
	e.addSession("-g", uuid, filepath.Join(e.root, "never-existed"), "a session with no capture")
	e.run("scan")

	res := e.run("restore", uuid[:8], "-n")
	if res.code == 0 {
		t.Error("restore of an uncapturable session exited 0")
	}
	if !strings.Contains(res.out(), "no remote and no archive") {
		t.Errorf("error should say why: %q", res.out())
	}
	if !strings.Contains(res.out(), "while they still existed") {
		t.Errorf("error should include the guidance: %q", res.out())
	}
}

func TestConfigRoundTripThroughCLI(t *testing.T) {
	e := newEnv(t)

	initial := e.run("config")
	if !strings.Contains(initial.stdout, "vault_dir") {
		t.Fatalf("config output = %q", initial.stdout)
	}

	newVault := filepath.Join(e.root, "elsewhere", "vault")
	set := e.run("config", "set", "vault_dir", newVault)
	if set.code != 0 {
		t.Fatalf("config set failed: %s", set.out())
	}

	after := e.run("config")
	if !strings.Contains(after.stdout, newVault) {
		t.Errorf("the new vault_dir did not persist: %q", after.stdout)
	}

	// An invalid value must be refused rather than written.
	bad := e.run("config", "set", "max_file_mb", "not-a-number")
	if bad.code == 0 {
		t.Error("config set accepted a non-numeric size")
	}
	if unknown := e.run("config", "set", "no_such_setting", "1"); unknown.code == 0 {
		t.Error("config set accepted an unknown key")
	}
}

func TestStatusAndGC(t *testing.T) {
	e := newEnv(t)
	live := e.liveDir("p")
	e.addSession("-p", "eeeeeeee-0000-0000-0000-000000000000", live, "a session")
	e.run("scan")

	st := e.run("status")
	for _, want := range []string{"vault", "sessions", "hooks"} {
		if !strings.Contains(st.stdout, want) {
			t.Errorf("status missing %q:\n%s", want, st.stdout)
		}
	}

	// gc on a healthy vault must find nothing and must not remove a transcript.
	gc := e.run("gc", "-n")
	if gc.code != 0 {
		t.Fatalf("gc dry run failed: %s", gc.out())
	}
	if !strings.Contains(gc.stdout, "nothing to collect") {
		t.Errorf("gc proposed work on a clean vault: %q", gc.stdout)
	}
}

// install-hooks must be idempotent and must not destroy existing settings.
func TestInstallHooksMergesAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	settings := filepath.Join(e.claudeHome, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	first := e.run("install-hooks")
	if first.code != 0 {
		t.Fatalf("install-hooks failed: %s", first.out())
	}
	if !strings.Contains(first.stdout, "added") {
		t.Errorf("expected hooks to be added: %q", first.stdout)
	}

	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("settings.json is no longer valid JSON: %v", err)
	}
	if parsed["theme"] != "dark" {
		t.Error("install-hooks destroyed an existing setting")
	}
	if _, ok := parsed["hooks"]; !ok {
		t.Error("no hooks block was written")
	}

	second := e.run("install-hooks")
	if strings.Contains(second.stdout, "added") {
		t.Errorf("second run should be a no-op: %q", second.stdout)
	}
	if !strings.Contains(second.stdout, "already installed") {
		t.Errorf("second run output = %q", second.stdout)
	}

	// Dry run must not touch the file.
	before, _ := os.ReadFile(settings)
	e.run("install-hooks", "-n")
	afterBytes, _ := os.ReadFile(settings)
	if string(before) != string(afterBytes) {
		t.Error("the dry run modified settings.json")
	}
}

// The hook runs inside a live Claude session: it must always exit 0, whatever
// it is fed, or a malformed payload becomes a broken session.
func TestCaptureHookAlwaysSucceeds(t *testing.T) {
	e := newEnv(t)
	live := e.liveDir("p")
	const uuid = "ffffffff-0000-0000-0000-000000000000"
	e.addSession("-p", uuid, live, "captured by hook")
	transcript := filepath.Join(e.claudeHome, "projects", "-p", uuid+".jsonl")

	payloads := []string{
		fmt.Sprintf(`{"session_id":%q,"transcript_path":%q,"cwd":%q,"hook_event_name":"SessionEnd"}`, uuid, transcript, live),
		`{"session_id":"","transcript_path":"","hook_event_name":"SessionStart"}`,
		`{"session_id":"nonexistent","transcript_path":"/no/such/file"}`,
		`not json at all`,
		``,
		`{"session_id":` + strings.Repeat("x", 5000) + `}`,
	}
	for i, payload := range payloads {
		cmd := exec.Command(binaryPath, "capture", "--hook")
		cmd.Env = e.vars()
		cmd.Stdin = strings.NewReader(payload)
		if err := cmd.Run(); err != nil {
			t.Errorf("payload %d made the hook fail: %v", i, err)
		}
	}

	// The well-formed one should have actually captured something.
	if ls := e.run("ls"); !strings.Contains(ls.stdout, uuid[:8]) {
		t.Errorf("the hook did not index the session: %q", ls.stdout)
	}
}

// forget removes ccvault's copies without touching Claude Code's own files.
func TestForgetLeavesClaudeCodeAlone(t *testing.T) {
	e := newEnv(t)
	live := e.liveDir("p")
	const uuid = "12121212-0000-0000-0000-000000000000"
	e.addSession("-p", uuid, live, "to be forgotten")
	transcript := filepath.Join(e.claudeHome, "projects", "-p", uuid+".jsonl")
	e.run("scan")

	res := e.run("forget", uuid[:8], "--yes")
	if res.code != 0 {
		t.Fatalf("forget failed: %s", res.out())
	}
	if ls := e.run("ls"); strings.Contains(ls.stdout, uuid[:8]) {
		t.Error("the session is still indexed")
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Errorf("forget deleted Claude Code's own transcript: %v", err)
	}
}
