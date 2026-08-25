# Contributing to ccvault

Thanks for taking a look. This is a small tool with a narrow job, and the bar for changes is
mostly "does it hold up against real Claude Code state, not just against our own tests".

## Getting set up

```sh
git clone https://github.com/sysn3rd/ccvault
cd ccvault
go build ./...
go test ./...
```

Requires Go 1.25+ and git. Tests never touch a real vault or `~/.claude` —
`CCVAULT_CLAUDE_HOME` and `CCVAULT_HOME` redirect both, and every test uses `t.TempDir()`.

Before opening a pull request, run everything CI runs:

```sh
./scripts/qa.sh           # ~18s: build, gofmt, vet, staticcheck, tests, race,
                          # govulncheck, cross-compile, coverage
./scripts/qa.sh --quick   # ~5s: skips race, govulncheck and cross-compile
```

A missing optional tool is reported as *skipped*, never as a pass — a green run you did not
actually perform is worse than a red one. Install them with:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

Or run the pieces individually:

```sh
gofmt -l .          # must print nothing
go vet ./...
staticcheck ./...
go test ./...
go test -race ./... # a hook and the reconcile timer share one SQLite file
GOOS=darwin GOARCH=arm64 go build ./cmd/ccvault    # if you touched platform-specific code
```

CI runs the same set on Linux and macOS. **Run the macOS build at least once** if you touch
anything platform-specific: the spawn tests once passed on Linux while covering nothing on
darwin, and only the macOS CI job caught it.

### Coverage

```sh
./scripts/coverage.sh            # ~68%
./scripts/coverage.sh out.prof   # ...and write a profile
```

Use the script rather than `go test -cover ./...`, which reports about 44% here and is
wrong in two ways. It attributes coverage only to the package under test, so `internal/index`
reads as 0% despite being exercised by nearly every other package. And it cannot see across
a process boundary, so every command handler reads as untested even though `cli_test.go`
drives all of them through the real binary. The script fixes both with `-coverpkg` and
`GOCOVERDIR`, and passes `-count=1` because a cached test result runs no binary and so
produces no coverage at all.

Coverage is a proxy, not a target. Prefer a test that pins behaviour someone could plausibly
break over one that lights up statements.

### Occasional checks

Not gated in CI, because on this codebase they are mostly noise — but worth running by hand
when you touch the relevant areas:

```sh
# Security scan. Most G204 "subprocess launched with variable" findings are inherent:
# ccvault exists to shell out to git and open terminals. The defence is validating what
# reaches them, not refusing to launch. Read past those; the rest is worth a look.
golangci-lint run --no-config -E gosec ./...

# Fuzz the transcript parser for longer than the weekly job does.
go test ./internal/transcript/ -run FuzzParse -fuzz FuzzParse -fuzztime 10m
```

A one-off `gosec` run is what found an argument-injection bug in the directory opener, so
these are worth doing occasionally even though continuous enforcement is not worth the
exclusion list it would need.

## Reporting a bug

Include:

- `ccvault version` output
- `claude --version`
- your OS
- `ccvault status` (it lists no session content, only counts and paths)

**Never paste transcript contents into an issue.** They contain whatever passed through your
session — file contents, command output, credentials. If a bug needs a transcript to
reproduce, say so and we will work out a redacted reproduction.

## Reporting a security issue

Please do not open a public issue. See [SECURITY.md](SECURITY.md).

## Invariants

These are load-bearing. Breaking one is usually silent, which is why they are written down.

**Never parse a project folder name.** Claude Code maps `/`, `.` and `_` all to `-`, so
`~/code/a.b_c` and `~/code/a-b-c` share one folder. Identity is the session UUID; location is
the `cwd` field read from inside the transcript records. `TestSlugCollisionKeepsSessionsDistinct`
guards this.

**A capture hook must never wedge a session.** `ccvault capture --hook` runs inside a live
Claude session. It exits 0 unconditionally, bounds its own work, and never prompts. If you
add work to the capture path, it must be cancellable and must fail open.

**A referenced transcript is never a garbage-collection candidate.** It is the only artefact
that cannot be regenerated from anything else. Patches, bundles and archives are all
expendable; transcripts are not.

**Never write a vault into a path that merely exists.** An unplugged drive leaves its
mountpoint behind as an empty directory. Creating a vault there produces a decoy that the
real vault is shadowed by on remount. `config.Check()` is the gate; use it.

**Captured values are not trusted input.** Remote URLs, branch names and commits come from
whatever `.git/config` was in a directory you opened. Git reads an argument beginning with
`-` as an option, and `--upload-pack=<command>` is arbitrary command execution. Anything
reaching `exec` is validated first — see `internal/restore/validate.go`.

**Size caps are reported, never silent.** If an archive skips a file, that exclusion is
recorded and surfaced at restore time. A backup that quietly omits things is worse than one
that refuses.

## Testing philosophy

Most of the interesting bugs in this project were at the seams with the outside world, not in
our own logic, and unit tests over our own code could not have caught any of them:

- Patches were silently corrupt for weeks because `strings.TrimSpace` removed the trailing
  newline git requires. The patch text *looked* right.
- `restore <id> -n` performed a real restore, because Go's `flag` package stops parsing at
  the first positional argument.
- Adding `--` to `git checkout --detach` broke it, because `--` means "paths follow" there.

So: **prefer tests that exercise the real thing.** The restore tests build actual local git
remotes and clone from them. The scheduler test XML-parses the generated launchd plist. When
you cannot run the real thing (macOS from Linux, an unmounted drive without root), test the
artefact you produce rather than asserting the behaviour you assume.

Tests are named for the behaviour they protect, and comments say *why* a case matters rather
than restating the code.

**Concurrency is tested for real.** `TestConcurrentCaptureAndScan` runs a reconcile pass and
capture hooks against one SQLite file through separate connections, which is the arrangement
that actually happens in production. Without it, `go test -race` passing would only prove the
tests are not racy.

**The CLI is tested through the binary.** `cmd/ccvault/cli_test.go` builds ccvault and runs
real subcommands against an isolated `HOME`. Every bug this project has had in the command
layer lived in the seam between the user and the code — a flag read as a positional, a dry
run that wrote to disk — and none were reachable by testing internals.

**The transcript parser is fuzzed.** It reads an undocumented format written by another
program, so `FuzzParse` asserts it is total: any bytes produce a session or an error, never a
panic. A panic there would surface as a broken Claude session, since the parser runs inside a
capture hook.

## Code style

Standard Go, `gofmt`-clean, `staticcheck`-clean. Beyond that:

- Comments explain **why**, not what. If a line needs explaining, the reason is usually a
  constraint from outside the codebase — say what it is.
- Errors are single clauses, lower case, no trailing punctuation. Longer guidance for humans
  goes in a typed error's method (see `restore.NotCapturedError`) or at the point of display.
- Anything destructive takes `-n`.
- `internal/` packages are free to change shape; there is no external API to keep stable.

## Pull requests

Small and focused is easier to review than large and comprehensive. Explain what real
situation prompted the change — this project's history is mostly "found by trying it against
an actual machine", and that framing helps.

If you change behaviour, update the affected docs in the same pull request:
[README.md](README.md) for anything a user sees, [DESIGN.md](DESIGN.md) for how it works.
