package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=v0.1.0"
//
// Left as "dev" for a plain `go build`, and filled in from module metadata when
// installed with `go install`, so a bug report always names something specific.
var version = "dev"

func versionString() string {
	v := version
	if v == "dev" {
		// A released build carries a real tag. Anything else is a source build,
		// and a go pseudo-version ("v0.0.0-2026...-abc123") tells a reader less
		// than "dev" plus the commit does.
		if info, ok := debug.ReadBuildInfo(); ok && isRelease(info.Main.Version) {
			v = info.Main.Version
		} else if revision := vcsRevision(); revision != "" {
			v = "dev (" + revision + ")"
		}
	}
	return fmt.Sprintf("ccvault %s — %s/%s, %s", v, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func isRelease(v string) bool {
	return v != "" && v != "(devel)" && !strings.HasPrefix(v, "v0.0.0-")
}

// vcsRevision is embedded by the Go toolchain when building inside a git
// checkout, which covers the common case of building from source.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 8 {
				revision = s.Value[:8]
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision != "" && dirty {
		revision += ", modified"
	}
	return revision
}
