package scheduler

import (
	"encoding/xml"
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
