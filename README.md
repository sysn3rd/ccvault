# ccvault

**Never lose a Claude Code conversation because you deleted the directory.**

Claude Code stores every conversation on disk, but its `/resume` picker only shows sessions
whose working directory matches where you launched it. Delete or rename that directory and
the conversation becomes unreachable — the data is still there, nothing surfaces it.

ccvault indexes every session regardless of directory, keeps its own copies, and records the
git provenance Claude Code never writes down. So you can search everything you have ever
worked on, and rebuild a deleted repository — at the right commit, with your uncommitted
changes replayed — then drop straight back into the conversation.

```
╭──────────────────────────────────────────────────────────────────────────╮
│ > auth▊                                                                  │
│ ──────────────────────────────────────────────────────────────────────── │
│ ● Refactor the auth middleware                          ~/code/api-server │
│     2h ago · repo main@a41c9e02 · OK                                     │
│   Debug the OAuth redirect loop                        ~/code/web-client │
│     3d ago · repo fix/oauth@71bd0c14 · MISSING                            │
│   Notes on session tokens                                       ~/scratch │
│     1w ago · plain · OK                                                  │
╰──────────────────────────────────────────────────────────────────────────╯
 12 session(s) · showing 1-3
 enter choose · , settings · ^o open dir · ^y copy id · esc quit
```

---

## Why this exists

Two things about Claude Code make this both necessary and possible:

**Resume is not directory-locked.** `claude --resume <session-id>` works from anywhere — only
the interactive picker is scoped to your current directory. So the missing capability was
never resumption; it was *finding* the session and *rebuilding* somewhere to resume it.

**Git provenance is never recorded.** Transcripts store the working directory and a bare
branch name — no remote URL, no commit SHA. Every session you run without ccvault is one
whose exact commit is unrecoverable. That is the deadline this tool is racing.

## Install

Requires **Go 1.25+** (pulled in by the pure-Go SQLite driver) and **git**. Works on Linux and macOS.

```sh
go install github.com/sysn3rd/ccvault/cmd/ccvault@latest
```

Or from source:

```sh
git clone https://github.com/sysn3rd/ccvault
cd ccvault
go build -o ~/.local/bin/ccvault ./cmd/ccvault
```

Make sure the install directory is on your `PATH`, then set it up:

```sh
ccvault install-hooks    # capture on session start and end
ccvault install-timer    # reconcile every 15 minutes
ccvault scan             # index everything already on disk
```

Both installers are idempotent, accept `-n` for a dry run, and back up
`~/.claude/settings.json` before touching it. `ccvault status` tells you where things stand.

## Quickstart

```sh
ccvault search           # interactive picker over every session
ccvault ls               # plain listing, newest first
ccvault status           # vault health
```

In the picker: type to filter, `enter` to choose what to do, `,` for settings, `esc` to quit.

> **Close the picker with `esc`,** not your window manager's close key. The TUI holds the
> terminal in raw mode, so a close request from outside does not reach it.

## Commands

| Command | What it does |
|---|---|
| `ccvault search [query]` | Interactive picker; plain lines when piped |
| `ccvault ls` | Every session, newest first |
| `ccvault show <id>` | One session in full: git state, patches, snapshots |
| `ccvault restore <id>` | Rebuild the directory, then resume |
| `ccvault scan` | Ingest new or grown transcripts; refresh directory state |
| `ccvault status` | Vault size, directory health, hook state |
| `ccvault config` | Show or change settings |
| `ccvault vault <init\|move\|status>` | Create, relocate or inspect the vault |
| `ccvault pending [adopt]` | Captures held while the vault was unavailable |
| `ccvault forget <id>` | Drop a session from the vault |
| `ccvault gc` | Reclaim space |
| `ccvault version` | Version, platform, Go version |

Session IDs accept any unambiguous prefix, so `ccvault show a41c9e02` is enough.

Anything destructive takes `-n` to preview: `restore`, `gc`, `install-hooks`, `install-timer`.

## Searching

Search covers session titles, first prompts, and the full text of what you typed — not tool
output, which is bulk rather than signal. Matching is incremental and mid-word.

```sh
ccvault search oauth              # opens the picker with the query filled in
ccvault search "oauth OR saml"    # piped or redirected: full FTS5 syntax, plain output
ccvault ls | grep MISSING
```

Piped output stays plain and passes your query to SQLite verbatim, so it composes in scripts.

## The action menu

`enter` opens a menu rather than guessing what you meant. Options that cannot work yet are
shown with the reason instead of hidden:

```
Debug the OAuth redirect loop
~/code/web-client  ·  MISSING

› Restore the directory — rebuild the directory, then resume
  Open in a new terminal  (unavailable)
      the directory no longer exists — restore it first
  Continue in this window  (unavailable)
      the directory no longer exists — restore it first
  Open the directory  (unavailable)
      there is nothing to open yet
  Copy the session id
```

**Open in a new terminal** matters if you launch the picker as a floating scratch window:
continuing in place would leave your session trapped in it. The new window gets its own
application id so it does not inherit the picker's window rules.

## Restoring a deleted directory

```sh
ccvault restore <id> -n              # show the plan, write nothing
ccvault restore <id>                 # rebuild, then resume
ccvault restore <id> --to ~/tmp/x    # rebuild somewhere else
ccvault restore <id> --no-resume     # rebuild only
ccvault restore <id> --fork          # resume under a new session id
```

```
restore a41c9e02  Refactor the auth middleware
  into     ~/code/api-server
  strategy clone
    · clone git@github.com:example/api-server.git
    · check out main at a41c9e02
    · replay uncommitted changes
    · restore untracked files
    · restore .claude/ (not tracked by the repo)

  ✓ cloned git@github.com:example/api-server.git
  ✓ checked out main at a41c9e02
  ✓ replayed uncommitted changes
  ✓ restored untracked files
  ✓ restored .claude/
```

Two strategies, chosen by what was captured:

- **clone** — a repository with a remote. Re-cloned at the exact commit, then your dirty
  patch, untracked files and (when the repo does not track it) `.claude/` are replayed on top.
- **snapshot** — anything else: a scratch directory, or a repository with no remote, whose
  archive then includes `.git` because nothing else holds its history.

Safeguards:

- The default target is the original directory, so Claude Code's own `/resume` works normally
  afterwards.
- A directory that already has contents is refused without `--force`. An empty one is fine.
- A commit that was never pushed is not on the remote — reported as a warning, with the clone
  left on its default branch, rather than a raw git error.
- A patch that will not apply warns and points at the patch file; a clone at the right commit
  is already most of the value.

## Directory states

| | |
|---|---|
| `OK` | the directory is there and has contents |
| `EMPTY` | it exists but holds nothing — resuming would land you nowhere useful |
| `MISSING` | deleted; must be rebuilt before it can be resumed |
| `NO TRANSCRIPT` | Claude Code pruned the conversation; the directory can still be rebuilt |

## Configuration

Settings live in `~/.config/ccvault/config.toml`. Edit by hand, from the CLI, or press `,` in
the picker.

```sh
ccvault config                                    # effective settings and vault health
ccvault config set vault_dir /mnt/backup/ccvault
ccvault config init                               # write a commented file
ccvault config path
```

```toml
vault_dir   = "/mnt/backup/ccvault"
claude_home = "/home/you/.claude"

[snapshots]
max_file_mb  = 16
max_total_mb = 256
ignore_dirs  = ["node_modules", ".venv", "target"]

[retention]
snapshots_kept  = 3
git_states_kept = 3

[picker]
terminal = ""   # blank auto-detects
```

**Settings belong in this file rather than environment variables**, and that is not a style
preference. ccvault runs from three places that do not share an environment: the CLI you
type, a hook spawned inside a Claude session, and the reconcile timer. An exported
`CCVAULT_HOME` reaches the hook — it inherits your shell — but not the timer, which has no
shell at all. The result is two half-vaults, each holding part of your history. The
environment variables still work for tests, and `ccvault config` warns when one is in effect.

## Backing up to an external drive

```sh
ccvault vault move /mnt/backup/ccvault
```

Copies, verifies the copy opens, then switches your settings over. The original is left in
place — deleting the only copy of something irreplaceable is your call.

When the drive is not there, ccvault stops and asks rather than guessing:

```
The vault is not available.

  /mnt/backup/ccvault is an empty mountpoint — the drive holding the vault is not mounted
  last seen 2h ago

  1 session(s) were captured while the vault was away, held at
  ~/.local/state/ccvault/pending (339.0 KB)
  They are safe. `ccvault pending adopt` files them once the vault is back.

What you can do:
  · reconnect the drive, then run the command again
  · point ccvault somewhere else:  ccvault config set vault_dir <path>
```

It distinguishes **an unmounted drive** (reconnect it) from **a deleted directory** (nothing
to reconnect; recreate empty with `ccvault vault init`) by remembering which filesystem the
vault was on, in local state that survives the drive going away.

It will not create a fresh vault inside an unmounted mountpoint. That decoy would be shadowed
by the real vault on remount, leaving two incomplete stores.

### Captures taken while the drive was away

A capture hook runs inside a live Claude session and can never stop to ask a question, so it
cannot prompt for a new location. Skipping would permanently lose the commit SHA and the
uncommitted diff. Instead it writes to a local holding area, and nothing leaves there until
you say so:

```sh
ccvault pending           # what is held, and from which sessions
ccvault pending adopt     # file it into the vault
ccvault pending discard   # throw it away (asks first)
```

## Reclaiming space

```sh
ccvault gc -n                    # show what would go
ccvault gc                       # collect
ccvault gc --older-than 720h     # also prune supporting files older than 30 days
```

gc removes orphaned files and git captures beyond the newest few per session. **A transcript
any session still references is never a candidate** — it is the one thing that cannot be
regenerated. Retention outranks age, so `--older-than` never strips the captures a restore
would actually use.

## How capture works

Two paths feed one pipeline:

- **Hooks** (`SessionStart`, `SessionEnd`) fire at the only moments accurate git state exists.
  `SessionStart` runs asynchronously so it cannot delay startup; `SessionEnd` runs
  synchronously with a 10 second cap. The capture command exits 0 unconditionally — a backup
  tool must never wedge your session.
- **A timer** runs `ccvault scan` every 15 minutes to reconcile what hooks miss: crashes,
  `kill -9`, and every session that predates installation. Unchanged transcripts are skipped
  on a size comparison, since they are append-only.

What is stored, in `~/.local/share/ccvault` (mode `0700`, files `0600`):

| | |
|---|---|
| `transcripts/<id>.jsonl` | full copy, so losing `~/.claude` does not lose context |
| `patches/<id>-<ts>.patch` | `git diff --binary HEAD`, in canonical `a/`,`b/` form |
| `patches/<id>-<ts>.untracked.tar.gz` | untracked file **contents** — `git diff` misses these |
| `snapshots/<treehash>.tar.gz` | whole-directory archive, for what git cannot cover |
| `index.db` | SQLite catalogue and full-text search index |

Archives are named by content, so sessions sharing a directory reference one file rather than
a copy each. Rebuildable directories (`node_modules`, `target`, `.venv`, …) are excluded, and
every exclusion is recorded and surfaced at restore time rather than silently dropped.

Sessions are also seeded from `~/.claude/history.jsonl`. Claude Code prunes transcripts after
`cleanupPeriodDays` (30 by default) but keeps its prompt log indefinitely, so those sessions
stay searchable — listed as `NO TRANSCRIPT`, since the conversation itself is gone.

## Platform notes

Linux and macOS use the same binary, the same layout and the same vault path
(`~/.local/share/ccvault` on both, deliberately, so a vault carried between machines drops in
unchanged). `install-timer` writes a systemd user timer or a launchd agent as appropriate.

### Omarchy / Hyprland keybinding

```lua
-- ~/.config/hypr/bindings.lua
o.bind("SUPER + R", "Resume Claude session", "omarchy-launch-or-focus-tui ccvault search")

-- ~/.config/hypr/hyprland.lua
o.window("org.omarchy.ccvault", { tag = "+floating-window" })
```

`launch or focus tui` reuses an already-open picker instead of stacking terminals, and the
`floating-window` tag inherits Omarchy's own float treatment rather than restating it.

## Privacy

**Transcripts contain everything that passed through a session** — file contents, command
output, environment values, anything you pasted. The vault is owner-only and nothing leaves
your machine: ccvault makes no network requests of its own, and the only network access it
ever causes is `git clone` during a restore, to a remote you already had.

There is no redaction. Treat the vault as being exactly as sensitive as the sessions it holds,
and think before putting it on a shared drive.

## Limitations

- **Sessions that predate installation cannot be fully restored.** ccvault can only rebuild
  directories it saw while they still existed. Install it before you need it.
- **A commit that was never pushed cannot be cloned back.** Restore warns and leaves you on
  the default branch. Push your work.
- **`.claude/` is only recovered when ccvault archived it.** If your repo tracks it, a clone
  brings it back anyway.
- **No encryption at rest yet**, and no sync between machines.
- **No redaction.** ccvault stores what Claude Code stored.

## Development

```sh
go test ./...            # everything
go build ./...
GOOS=darwin GOARCH=arm64 go build ./cmd/ccvault    # cross-compile check
```

Tests use synthetic fixtures and never touch a real `~/.claude` or vault —
`CCVAULT_CLAUDE_HOME` and `CCVAULT_HOME` redirect both.

See [DESIGN.md](DESIGN.md) for how it works internally, and for the two undocumented Claude
Code behaviours that shaped the design. [CONTRIBUTING.md](CONTRIBUTING.md) covers the
invariants worth knowing before changing anything.

Contributions are welcome. Security issues should go through
[SECURITY.md](SECURITY.md) rather than a public issue.

## Licence

Apache License 2.0 — see [LICENSE](LICENSE). Use it freely; keep the copyright notice and
mark any files you change.

Not affiliated with Anthropic. "Claude" and "Claude Code" are trademarks of Anthropic.
