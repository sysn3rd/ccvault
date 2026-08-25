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

Phases 1 (capture), 2 (search) and 3 (restore) are complete and running.
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
│   Custom theme for Omarchy OS                                     ~/Work │
│     1d ago · plain · OK                                                  │
│   Phase 3 task groups                             ~/code/TheLastAssembly │
│     33m ago · repo main@a2ac4b91 · OK                                    │
╰──────────────────────────────────────────────────────────────────────────╯
 8 session(s) · showing 1-3
 enter resume · ^o open dir · ^y copy id · ^u clear · esc quit
```

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
