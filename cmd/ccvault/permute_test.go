package main

import (
	"flag"
	"io"
	"testing"
)

// `ccvault restore <uuid> -n` must be a preview. Go's flag package stops at the
// first positional, so without permuting, -n is swallowed and a real restore
// runs when the user asked to see the plan.
func TestPermutePutsFlagsBeforePositionals(t *testing.T) {
	newFS := func() (*flag.FlagSet, *bool, *string) {
		fs := flag.NewFlagSet("restore", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dry := fs.Bool("n", false, "")
		to := fs.String("to", "", "")
		return fs, dry, to
	}

	cases := []struct {
		name     string
		args     []string
		wantDry  bool
		wantTo   string
		wantArg0 string
	}{
		{"flag after positional", []string{"abc123", "-n"}, true, "", "abc123"},
		{"flag before positional", []string{"-n", "abc123"}, true, "", "abc123"},
		{"valued flag after positional", []string{"abc123", "-to", "/tmp/x"}, false, "/tmp/x", "abc123"},
		{"inline value", []string{"abc123", "-to=/tmp/x"}, false, "/tmp/x", "abc123"},
		{"both", []string{"abc123", "-to", "/tmp/x", "-n"}, true, "/tmp/x", "abc123"},
		{"positional only", []string{"abc123"}, false, "", "abc123"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, dry, to := newFS()
			if err := fs.Parse(permute(fs, c.args)); err != nil {
				t.Fatal(err)
			}
			if *dry != c.wantDry {
				t.Errorf("-n = %v, want %v", *dry, c.wantDry)
			}
			if *to != c.wantTo {
				t.Errorf("-to = %q, want %q", *to, c.wantTo)
			}
			if fs.Arg(0) != c.wantArg0 {
				t.Errorf("Arg(0) = %q, want %q", fs.Arg(0), c.wantArg0)
			}
		})
	}
}

// Everything after -- is positional, even if it looks like a flag.
func TestPermuteRespectsDoubleDash(t *testing.T) {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("n", false, "")
	if err := fs.Parse(permute(fs, []string{"--", "-n"})); err != nil {
		t.Fatal(err)
	}
	if *dry {
		t.Error("-n after -- should be positional, not a flag")
	}
	if fs.Arg(0) != "-n" {
		t.Errorf("Arg(0) = %q, want -n", fs.Arg(0))
	}
}
