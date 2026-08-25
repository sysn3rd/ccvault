# ccvault

Inventory, backup and restore for Claude Code session context.

Claude Code stores each conversation as an append-only JSONL transcript under
`~/.claude/projects/<slug-of-cwd>/<session-uuid>.jsonl`. The `/resume` picker only lists
sessions whose recorded `cwd` matches where you launched Claude — so when a working
directory is deleted or renamed, its conversations become unreachable even though the data
is still on disk.

ccvault indexes every session regardless of cwd, keeps its own copies, and records the git
provenance Claude Code never writes down, so a deleted repository can be rebuilt and
resumed.

## Status

All four planned phases are complete and running: capture, search, restore and polish.
See [VISION.md](VISION.md).

## Install

```sh
go build -o ~/.local/bin/ccvault ./cmd/ccvault
ccvault install-hooks    # merge capture hooks into ~/.claude/settings.json
ccvault install-timer    # systemd user timer (Linux) or launchd agent (macOS)
ccvault scan             # backfill everything that already exists
```

Both installers are idempotent, take `-n` for a dry run, and back up `settings.json` before
touching it.

## Use

```sh
ccvault search               # interactive picker over every session
ccvault search omar          # ...opened with a query already typed
ccvault ls                   # plain listing, newest first
ccvault show <uuid>          # metadata, git provenance, dirty patch, bundles
ccvault restore <uuid>       # rebuild the directory, then resume
ccvault forget <uuid>        # drop a session from the vault
ccvault gc                   # reclaim space
ccvault status               # vault size, directory health, hook health
```

UUID prefixes work everywhere a uuid is accepted, so `ccvault restore 9b782f76` is enough.

## Restore

```sh
ccvault restore <uuid> -n           # show the plan, write nothing
ccvault restore <uuid>              # rebuild, then resume
ccvault restore <uuid> --to ~/tmp/x # rebuild somewhere else
ccvault restore <uuid> --no-resume  # rebuild only
ccvault restore <uuid> --fork       # resume into a new session id
```

```
restore bd80b213  say only: E2E2
  into     ~/code/project
  strategy clone
    · clone git@github.com:sysn3rd/project.git
    · check out main at 25a062c8
    · replay uncommitted changes
    · restore untracked files
    · restore .claude/ (not tracked by the repo)

  ✓ cloned git@github.com:sysn3rd/project.git
  ✓ checked out main at 25a062c8
  ✓ replayed uncommitted changes
  ✓ restored untracked files
  ✓ restored .claude/
```

Two strategies, chosen by what was captured:

- **clone** — a repository with a remote. Re-cloned at the exact commit, then the dirty
  patch, the untracked files and (when the repo does not track it) `.claude/` are replayed
  on top.
- **snapshot** — anything else: a scratch workspace, or a repository with no remote, whose
  archive then includes `.git` because nothing else holds its history.

Safety:

- The default target is the original directory, so Claude Code'"'"'s own `/resume` picker
  works normally afterwards.
- A directory that already has contents is refused unless you pass `--force`. An empty one
  is fine — that is the common case.
- A commit that was never pushed is not on the remote. That is reported as a warning and
  the clone is left on its default branch, rather than failing with a raw git error.
- A patch that will not apply warns and points at the patch file; the clone at the right
  commit is already most of the value.

### The picker

```
╭──────────────────────────────────────────────────────────────────────────╮
│ > omar▊                                                                  │
│ ──────────────────────────────────────────────────────────────────────── │
│ ● Omarchy OS driver setup                                         ~/Work │
│     1d ago · plain · OK                                                  │
│   Phase 3 task groups                             ~/code/TheLastAssembly │
│     33m ago · repo main@a2ac4b91 · OK                                    │
╰──────────────────────────────────────────────────────────────────────────╯
 8 session(s) · showing 1-2
 enter choose · , settings · ^o open dir · ^y copy id · esc quit
```

`enter` opens an action menu rather than guessing what you meant. Options that cannot work
yet are shown with the reason, not hidden:

```
Kali Linux virtual desktop setup          │  (no prompts)
~/Work  ·  OK                             │  ~/code/games  ·  EMPTY
                                          │
› Open in a new terminal — leaves this    │  › Restore the directory — rebuild, then resume
  Continue in this window — replaces it   │    Open in a new terminal  (unavailable)
  Restore the directory  (unavailable)    │        the directory is empty — restore it first
      the directory is already there      │    Continue in this window  (unavailable)
  Open the directory                      │        the directory is empty — restore it first
  Copy the session id                     │    Copy the session id
```

**Open in a new terminal** matters when the picker is itself a floating scratch window:
continuing in place would leave your session trapped in it. The new window gets its own
app id, so it does not inherit the picker'"'"'s window rules.

Press `,` for the settings editor — the same values as the TOML file, edited in place,
`^s` to save.

Search is incremental and matches mid-word, so every term becomes an FTS5 prefix query.
Arbitrary punctuation is quoted rather than passed to the FTS parser, where a stray `"` or
`(` would be a syntax error instead of a search.

`enter` hands the terminal to `claude --resume <uuid>` in the session'"'"'s directory —
resumption is not reimplemented, because `--resume` already works from anywhere. Two cases
Enter declines, with an explanation rather than a failure: a directory that no longer exists
(that is phase 3) and a session Claude Code has already pruned.

**Piped output stays plain**, and passes the query to FTS verbatim so boolean syntax works
in scripts:

```sh
ccvault search "kali OR pentest" | head
ccvault ls | grep MISSING
```

## How capture works

Two paths feed one pipeline:

- **Hooks** (`SessionStart`, `SessionEnd`) fire at the only moments accurate git state
  exists. `SessionStart` runs `async` so it cannot delay startup; `SessionEnd` runs
  synchronously with a 10s cap because an async hook can lose the race with process exit.
  The `capture` command exits 0 unconditionally — a backup tool must never wedge a session.
- **A timer** runs `ccvault scan` every 15 minutes to reconcile what hooks miss: crashes,
  `kill -9`, and every session that predates installation. Unchanged transcripts are skipped
  on a size comparison, since they are append-only.

What gets stored per session, in `~/.local/share/ccvault` (mode `0700`, files `0600`):

| | |
|---|---|
| `transcripts/<uuid>.jsonl` | full copy, so losing `~/.claude` does not lose context |
| `patches/<uuid>-<ts>.patch` | `git diff --binary HEAD`, forced to canonical `a/`,`b/` prefixes |
| `patches/<uuid>-<ts>.untracked.tar.gz` | untracked file **contents** — `git diff` does not cover these |
| `snapshots/<treehash>.tar.gz` | whole-directory archive, for what git cannot cover |
| `index.db` | SQLite catalogue + FTS5 search index |

Archives are named by content, so the nine sessions that share `~/Work` reference one file
rather than nine copies of it. Rebuildable directories (`node_modules`, `target`, `.venv`, …)
are excluded, and every exclusion is recorded and surfaced at restore time rather than
silently dropped.

Sessions are also seeded from `~/.claude/history.jsonl`. Claude Code prunes transcripts
after `cleanupPeriodDays` (30 by default) but keeps the prompt log forever, so those
sessions stay searchable — listed as `NO TRANSCRIPT`, since the context itself is gone.

Git provenance recorded: remote URL, branch, commit SHA, dirty flag, submodules, and
whether `.claude/` is tracked in the repo (an untracked one is silently lost by a
clone-based restore, taking the project's skills and agents with it).

## Two directory kinds

`ccvault ls` labels every session `repo` or `plain`, because they restore differently:

- **`repo`** — a git worktree with a remote. Rebuildable from remote + SHA + dirty patch +
  untracked bundle. Cheap to store.
- **`plain`** — not a repo at all (a scratch workspace like `~/Work`). No remote to clone,
  so restore means a tree snapshot.

## Directory states

| | |
|---|---|
| `OK` | the directory is there and has contents |
| `EMPTY` | it exists but holds nothing — resuming lands you somewhere useless, so Enter restores |
| `MISSING` | deleted; must be rebuilt before it can be resumed |
| `NO TRANSCRIPT` | Claude Code pruned the conversation; the directory can still be rebuilt |

## Settings

Settings live in `~/.config/ccvault/config.toml`, editable by hand, from the CLI, or in the
picker (press `,`).

```sh
ccvault config                          # show effective settings and vault health
ccvault config set vault_dir /mnt/backup/ccvault
ccvault config init                     # write a commented file with current values
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

**Settings belong in this file rather than the environment**, and that is not a style
preference. ccvault runs from three places that do not share an environment: the CLI you
type, a hook spawned inside a Claude session, and the reconcile timer. An exported
`CCVAULT_HOME` reaches the hook — it inherits your shell — but not the timer, which has no
shell at all. The result is two half-vaults, each holding part of your history. The env
vars still work for tests, and `ccvault config` warns loudly when one is in effect.

## Backing up to an external drive

```sh
ccvault vault move /mnt/backup/ccvault   # copy, verify, then switch settings over
```

The original is left in place; deleting the only copy of something irreplaceable is your
call. `ccvault vault status` reports health at any time.

When the drive is not there, ccvault **stops and asks** rather than guessing:

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
vault was on, in local state that survives the drive going away. Critically, it will not
create a fresh vault inside an unmounted mountpoint — that decoy would be shadowed by the
real vault on remount, leaving two incomplete stores.

### Captures taken while the drive was away

A hook runs inside a live Claude session and can never stop to ask a question, so it cannot
prompt for a new location. Skipping would permanently lose the commit SHA and the
uncommitted diff. Instead it writes to a local holding area, and nothing leaves there until
you say so:

```sh
ccvault pending           # what is held, and from which sessions
ccvault pending adopt     # file it all into the vault
ccvault pending discard   # throw it away (asks first)
```

## Reclaiming space

```sh
ccvault gc -n                    # show what would go
ccvault gc                       # collect
ccvault gc --older-than 720h     # also prune supporting files older than 30 days
```

gc removes orphaned files (whatever `forget` or a manual deletion left behind) and git
captures beyond the newest three per session. **A transcript any session still references
is never a candidate** — it is the one thing that cannot be regenerated. Retention outranks
age, so `--older-than` never strips the captures a restore would actually use.

## Omarchy keybinding

`SUPER + R` ("resume") opens the picker in a floating terminal:

```lua
-- ~/.config/hypr/bindings.lua
o.bind("SUPER + R", "Resume Claude session", "omarchy-launch-or-focus-tui ccvault search")

-- ~/.config/hypr/hyprland.lua
o.window("org.omarchy.ccvault", { tag = "+floating-window" })
```

`launch or focus tui` reuses an already-open picker instead of stacking terminals, and the
`floating-window` tag inherits Omarchy'"'"'s own treatment (float, centered, 875x600) rather
than restating those rules. Close the picker with `esc` — the TUI holds the terminal in raw
mode, so a window-manager close request does not reach it.

## macOS

The same binary, the same layout. `install-timer` writes a launchd agent instead of a
systemd timer, `^o` uses `open` instead of `xdg-open`, and the clipboard falls back through
`wl-copy` → `xclip` → `pbcopy`. The vault lives at `~/.local/share/ccvault` on both
platforms deliberately, so the two machines stay symmetric.

```sh
GOOS=darwin GOARCH=arm64 go build -o ccvault ./cmd/ccvault
```

The generated launchd plist is XML-parsed in tests, so its structure is verified from Linux
rather than assumed.

## The lossy-slug trap

Claude Code maps `/`, `.` and `_` all to `-` when naming a project folder. Verified: a
session in `a.b_c/` and one in `a-b-c/` land in the **same** folder, transcripts
interleaved.

> Identity is the session UUID. Location is the `cwd` read from inside the transcript
> records. The folder name is an opaque shard and is never parsed.

`TestSlugCollisionKeepsSessionsDistinct` guards this.

## A session can span several directories

`claude --resume <uuid>` works from any directory and appends to the original transcript, so
one file can carry more than one `cwd`. ccvault keeps the first as the session's home and
records the rest; `ccvault show` lists them.

## Privacy

Transcripts contain full file contents, command output and anything else that passed
through a session. The vault is owner-only and nothing leaves the machine. There is no
redaction yet — treat the vault as being as sensitive as the sessions it holds.

## Tests

```sh
go test ./...
```

Fixtures are synthetic; no test touches the real `~/.claude` or vault (`CCVAULT_CLAUDE_HOME`
and `CCVAULT_HOME` redirect both).
