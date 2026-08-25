package scheduler

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemdUnitsAreWellFormed(t *testing.T) {
	service, timer := RenderSystemd("/home/u/.local/bin/ccvault")

	for _, want := range []string{
		"[Service]",
		"ExecStart=/home/u/.local/bin/ccvault scan",
		"Type=oneshot",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service unit missing %q", want)
		}
	}
	for _, want := range []string{
		"[Timer]",
		"[Install]",
		"WantedBy=timers.target",
		// Without Persistent, a laptop that was asleep at the scheduled moment
		// silently skips the run instead of catching up.
		"Persistent=true",
	} {
		if !strings.Contains(timer, want) {
			t.Errorf("timer unit missing %q", want)
		}
	}
}

// The macOS agent cannot be exercised from Linux, so its structure is checked
// by parsing it rather than assumed correct.
func TestLaunchdPlistParsesAndCarriesTheRightKeys(t *testing.T) {
	plist := RenderLaunchd("/usr/local/bin/ccvault")

	var parsed struct {
		XMLName xml.Name `xml:"plist"`
		Dict    struct {
			Keys    []string `xml:"key"`
			Strings []string `xml:"string"`
			Ints    []int    `xml:"integer"`
			Arrays  []struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(plist), &parsed); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}

	keys := strings.Join(parsed.Dict.Keys, ",")
	for _, want := range []string{"Label", "ProgramArguments", "StartInterval", "RunAtLoad"} {
		if !strings.Contains(keys, want) {
			t.Errorf("plist missing key %q (has: %s)", want, keys)
		}
	}
	if len(parsed.Dict.Arrays) == 0 {
		t.Fatal("ProgramArguments array is missing")
	}
	args := parsed.Dict.Arrays[0].Strings
	if len(args) != 2 || args[0] != "/usr/local/bin/ccvault" || args[1] != "scan" {
		t.Errorf("ProgramArguments = %v, want [/usr/local/bin/ccvault scan]", args)
	}
	var foundInterval bool
	for _, n := range parsed.Dict.Ints {
		if n == IntervalSeconds {
			foundInterval = true
		}
	}
	if !foundInterval {
		t.Errorf("StartInterval %d not present in %v", IntervalSeconds, parsed.Dict.Ints)
	}
	if !strings.Contains(plist, LaunchdLabel) {
		t.Errorf("plist does not carry the label %q", LaunchdLabel)
	}
}

// A path with a space would break the plist and the unit file alike if it were
// ever interpolated somewhere quoted incorrectly.
func TestRenderersHandleSpacesInPaths(t *testing.T) {
	const p = "/Applications/My Tools/ccvault"
	service, _ := RenderSystemd(p)
	if !strings.Contains(service, "ExecStart="+p+" scan") {
		t.Errorf("systemd ExecStart mangled a path with a space:\n%s", service)
	}
	plist := RenderLaunchd(p)
	if err := xml.Unmarshal([]byte(plist), new(struct{})); err != nil {
		t.Errorf("plist with a spaced path is invalid XML: %v", err)
	}
	if !strings.Contains(plist, "<string>"+p+"</string>") {
		t.Error("plist did not carry the spaced path as its own argument")
	}
}

// Install writes into the user's home, so the tests redirect it there. The
// activation step (systemctl / launchctl) will fail under test, and that is the
// point: a scheduler that cannot be activated must still report what it wrote
// and how to finish, rather than failing silently or claiming success.
func TestInstallWritesUnitsAndReportsHonestly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd paths are Linux-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// An empty PATH makes systemctl unavailable, standing in for a machine
	// without systemd.
	t.Setenv("PATH", t.TempDir())

	plan, err := Install("/usr/local/bin/ccvault", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 2 {
		t.Fatalf("Files = %v, want a service and a timer", plan.Files)
	}
	for _, f := range plan.Files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("unit not written: %v", err)
		}
		if !strings.HasPrefix(f, home) {
			t.Errorf("unit written outside the redirected home: %s", f)
		}
	}
	if plan.Enabled {
		t.Error("Enabled = true even though systemctl was unavailable")
	}
	if plan.Hint == "" {
		t.Error("a failed activation must explain how to finish by hand")
	}
	if !strings.Contains(plan.Hint, "systemctl") {
		t.Errorf("Hint should name the command to run: %q", plan.Hint)
	}
}

// A dry run must describe the work without doing any of it.
func TestInstallDryRunWritesNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd paths are Linux-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	plan, err := Install("/usr/local/bin/ccvault", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) == 0 {
		t.Fatal("a dry run should still report what it would write")
	}
	for _, f := range plan.Files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("the dry run wrote %s", f)
		}
	}
	if plan.Enabled {
		t.Error("a dry run must not report the timer as active")
	}
}

// Reinstalling over an existing unit must succeed rather than refuse.
func TestInstallIsRepeatable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd paths are Linux-only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	for i := range 2 {
		if _, err := Install("/usr/local/bin/ccvault", false); err != nil {
			t.Fatalf("install %d failed: %v", i+1, err)
		}
	}
	units, err := os.ReadDir(filepath.Join(home, ".config", "systemd", "user"))
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 2 {
		t.Errorf("got %d units after two installs, want 2", len(units))
	}
}

// An unsupported platform should say so rather than write units nothing reads.
func TestUnsupportedPlatformIsAnError(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		t.Skip("this platform is supported")
	}
	if _, err := Install("/usr/local/bin/ccvault", true); err == nil {
		t.Error("expected an error on an unsupported platform")
	}
}
