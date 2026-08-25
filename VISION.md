# ccvault — backup and inventory for Claude Code context

## Context

Claude Code stores every conversation as an append-only JSONL transcript under
`~/.claude/projects/<slug-of-cwd>/<session-uuid>.jsonl`. The `/resume` picker only lists
sessions whose recorded `cwd` matches where you launched Claude, so once a working
directory is deleted or renamed, its conversations become effectively unreachable — the
data is still on disk, but nothing surfaces it.

This machine already has the problem: `~/code/games` is an empty directory with two
archived sessions behind it, one titled *"Fun game ideas"*.

Two findings from mapping the on-disk state shape the whole design:

1. **Resume is not cwd-locked.** `claude --resume <uuid>` works from any directory —
   verified by resuming a `~/Work` session from a `/tmp` scratch dir and getting full
   prior context back. So the missing capability is *discovery and reconstruction*, not
   resumption. ccvault never needs to reimplement resume; it needs to find the session
   and rebuild a directory worth resuming into.

2. **Git provenance is never recorded.** Transcripts carry `cwd`, `gitBranch`, `version`,
   `sessionId` and `ai-title`, but no remote URL and no commit SHA. `gitBranch` reads
   `"HEAD"` for non-repo directories. Every session run before capture exists is one whose
   exact commit is unrecoverable. This is the deadline on the project.

**Outcome:** a single Go binary that continuously indexes and vaults every Claude Code
session on this machine, lets you fuzzy-search all of them regardless of cwd, and can
rebuild a deleted working directory — cloning the repo to the right commit, or unpacking a
tree snapshot — then drop you back into the conversation.

## Decisions

| | |
|---|---|
| Language | Go — single static binary, no runtime deps, fast enough to run as a per-session hook |
| Interface | CLI subcommands + TUI fuzzy picker (bubbletea); plain output when piped |
| Storage | Local vault, full copies. Nothing leaves the machine. |
| Name | `ccvault` |
| Repo | `~/code/ccvault` |
| Platforms | Omarchy (primary) and macOS |

## Non-obvious constraints

**Project folder names are lossy — never reverse them.** `/`, `.` and `_` all collapse to
`-` in the slug. Verified empirically: `slug.test_dir` and `slug-test-dir` produced *one*
shared project folder holding both sessions' transcripts. With `~/code` repo naming this
will collide in practice (`agent-os` vs `agent.os`, `my_repo` vs `my-repo`).

> **Invariant:** identity is `(session_uuid)`; location is the `cwd` field read from inside
> the transcript records. The slug is an opaque shard name and must never be parsed.

**Two root types need two restore strategies.**

- `~/code/*` — real git worktrees. `TheLastAssembly` has
  `origin git@github.com:sysn3rd/TheLastAssembly.git`, `main` @ `a2ac4b9`, clean, no
  submodules. Reconstructible from remote + SHA + a dirty-state patch. Cheap.
- `~/Work` — not a repo at all (no `.git`; every record reads `gitBranch: "HEAD"`), and it
  holds the majority of sessions. No remote to clone, so restore means a tree snapshot,
  which needs size caps and an ignore list.

**Project-local `.claude/` may or may not survive a clone.** TheLastAssembly tracks its
`.claude/` (30 files: `agents/agent-os`, `commands`, 12 skills), so it returns with the
clone for free. That is a property of that repo, not a guarantee — when `.claude/` is
untracked or ignored, restoring without it silently strips the session's skills and agents.
Detect and snapshot separately.

**Transcripts contain secrets** — full file contents, command output, tokens. Vault is
`0700`, files `0600`, no network path in v1.

## Architecture

```
 Claude Code session
        │
        ├─ SessionStart hook ─┐
        ├─ SessionEnd hook  ──┼──> ccvault capture --hook   (git provenance, exact moment)
        │                     │
        └─ writes transcript ─┘
                                       ~/.claude/projects/**/*.jsonl
                                                  │
   systemd timer / launchd ──> ccvault scan  <────┘   (reconcile: crashes, pre-install
                                     │                 sessions, transcript growth)
                                     ▼
                        ~/.local/share/ccvault/
                          index.db          SQLite + FTS5
                          transcripts/<uuid>.jsonl
                          patches/<uuid>-<ts>.patch
                          snapshots/<uuid>-<ts>.tar.zst
                                     │
                    ccvault search ──┤── TUI picker
                    ccvault restore ─┴── rebuild dir → exec claude --resume <uuid>
```

Capture is belt-and-braces on purpose: hooks give exact git state at the moment a session
starts and ends; the timer catches everything hooks miss (crashes, `kill -9`, sessions that
predate installation) and picks up transcript growth mid-session.

### Hook payload

Confirmed present in the installed build (2.1.240): all ten hook events exist, and hook
JSON carries `session_id`, `transcript_path`, `cwd`, `hook_event_name`, `permission_mode`.
`SessionEnd` reasons are `clear`, `logout`, `prompt_input_exit`, `other`.

The hook must be non-blocking and fail-open — a slow or broken ccvault must never wedge a
Claude session. Target < 50ms, wrap all work in a timeout, exit 0 unconditionally.

## Data model

```sql
sessions(
  uuid TEXT PRIMARY KEY,        -- the only stable identity
  cwd TEXT NOT NULL,            -- read from transcript records, never from slug
  slug TEXT,                    -- opaque, for locating the source file only
  title TEXT,                   -- from ai-title records
  first_prompt TEXT,
  started_at, last_active INTEGER,
  kind TEXT,                    -- 'repo' | 'plain'
  msg_count INTEGER,
  cc_version TEXT,
  transcript_bytes INTEGER,     -- append-only: cheap change detection
  transcript_sha256 TEXT,
  dir_state TEXT                -- 'ok' | 'missing' | 'changed'
)

git_state(                      -- one row per capture event, keeps history
  session_uuid, captured_at, event,   -- 'start' | 'end' | 'scan'
  worktree_root, remote_url, branch, head_sha,
  is_dirty INTEGER, patch_path TEXT, untracked_path TEXT,
  submodules_json TEXT, claude_dir_tracked INTEGER
)

snapshots(session_uuid, captured_at, kind, path, bytes, sha256)

sessions_fts USING fts5(uuid, title, first_prompt, body)  -- body = concatenated user turns
```

`~/.claude/history.jsonl` (`display`, `timestamp`, `project`, `sessionId`) is a ready-made
global prompt corpus — seed FTS from it, then enrich from transcripts.

## Commands

| Command | Does |
|---|---|
| `ccvault capture --hook` | Reads hook JSON on stdin. Records git provenance, syncs transcript. Fail-open. |
| `ccvault scan` | Reconcile pass: ingest unknown sessions, refresh grown transcripts, mark `dir_state`. |
| `ccvault search [query]` | TUI picker on a tty, line output when piped. Enter = restore + resume. |
| `ccvault ls` | Table of all sessions with cwd, kind, git state, `dir_state`. |
| `ccvault show <uuid>` | Full metadata + git history + transcript stats. |
| `ccvault restore <uuid> [--to PATH] [--no-resume] [--fork]` | Rebuild directory, then exec resume. |
| `ccvault status` | Vault size, session counts, orphans, missing dirs, hook health. |
| `ccvault install-hooks` | Idempotent merge into `~/.claude/settings.json` + timer/launchd unit. |
| `ccvault gc [--older-than]` | Prune snapshots/patches by age or size budget. |

### Restore semantics

- **Default target is the original `cwd`.** If it's free, restore there — then the built-in
  `/resume` picker works naturally afterward and ccvault stays a thin index rather than a
  permanent shim. `--to` overrides.
- `kind=repo`: clone `remote_url` → checkout `head_sha` → recreate `branch` → apply dirty
  patch → unpack untracked bundle → if `claude_dir_tracked=0`, restore `.claude/` snapshot.
- `kind=plain`: unpack tree snapshot.
- Then `exec claude --resume <uuid>` in the restored directory. `--fork` adds
  `--fork-session` when you want the archived transcript left untouched; plain resume
  appends to the original, which is fine because the vault holds an immutable copy.
- Refuse to write into a non-empty directory without `--force`.

### Snapshot policy (`kind=plain`)

Config-driven, `~/.config/ccvault/config.toml`. Defaults: skip `node_modules`, `.venv`,
`target`, `build`, `dist`, `.cache`; honor `.gitignore` where present; per-snapshot size cap
256 MB with a loud warning and a recorded manifest when exceeded (never a silent truncation);
gzip tarballs (stdlib, no dependency — the plan said zstd, but that would pull in a
compression library for marginal gain at these sizes); dedupe by content hash so idle
directories don't re-snapshot.

## Build phases

**Phase 1 — Capture + index.** ✅ Done. Time-critical; every session run before this lands
loses its SHA forever. Two capture bugs surfaced during implementation and are fixed:
patches are forced to canonical `a/`,`b/` prefixes (local `diff.mnemonicPrefix` config
otherwise emits `c/`,`w/`, which may not apply on the other machine), and untracked file
*contents* are archived — `git diff HEAD` covers tracked files only, so they were subject to
the same one-shot loss as an uncaptured SHA. Go module scaffold, SQLite schema, transcript parser (tolerant: pin nothing to
`version`, ignore unknown record types), `capture --hook`, `scan`, `install-hooks`, systemd
timer + launchd plist. Ship this alone and it's already useful.

**Phase 2 — Search.** ✅ Done. FTS5 index seeded from transcripts and from `history.jsonl`
(which outlives pruned transcripts, so those sessions stay findable as `NO TRANSCRIPT`);
`ls` / `show` / `search`; bubbletea picker matching the planned layout. Incremental search
turns each term into a quoted FTS5 prefix query — passing raw input to the FTS parser makes
ordinary punctuation a syntax error rather than a search. Enter hands off to
`claude --resume`, and declines with an explanation for the two cases it cannot serve
(deleted directory, pruned transcript).

**Phase 3 — Restore.** Repo path first (cheap, high-confidence), then plain-tree snapshots.
Dirty-patch capture and replay. `.claude/` fallback handling.

**Phase 4 — Polish.** `status`, `gc`, macOS parity pass, Omarchy keybinding to launch the
picker, README.

## Verification

Real fixtures already exist on this machine — no synthetic test data needed:

| Case | Fixture | Expected |
|---|---|---|
| Repo restore | `~/code/TheLastAssembly` (clean, `main@a2ac4b9`) | Clone to temp, land on same SHA, `.claude/` present, resume answers a question about prior context |
| Plain restore | `~/Work` (no `.git`, most sessions) | Snapshot → wipe temp copy → unpack → tree matches by hash |
| Missing dir | `~/code/games` (empty, 2 sessions, "Fun game ideas") | Shows `MISSING`; restore reconstructs and resumes |
| Slug collision | Create `a.b_c/` and `a-b-c/`, run a session in each | Two distinct rows, correct distinct `cwd`s, no cross-contamination — **regression test, this is the sharpest edge** |
| Cross-dir resume | Any session | `ccvault restore --no-resume` then manual `claude --resume <uuid>` from elsewhere still loads context |
| Hook safety | `capture --hook` with a corrupt/huge stdin | Exits 0 within timeout; Claude session unaffected |

End-to-end smoke: `ccvault install-hooks` → start a session in a throwaway git repo → make
a commit and leave the tree dirty → exit → `rm -rf` the directory → `ccvault search` finds
it → Enter restores the repo at the right SHA with the dirty patch applied and resumes.

## Prerequisites

- `mise use -g go@latest` — Go is not currently installed (mise has `claude`, `codex`, `gh`, `node`).
- `~/.claude/settings.json` currently has no `hooks` block; `install-hooks` must merge into
  the existing keys (`skipDangerousModePermissionPrompt`, `theme`, `tui`), not overwrite.

## Deferred (explicitly out of scope for v1)

- Encrypted off-machine sync between Omarchy and the Mac — the vault design doesn't preclude
  it; revisit once the local flow is proven.
- Secret redaction inside archived transcripts.
- Web UI for reading old conversations.
- Restoring MCP server state or `~/.claude.json` per-project trust entries — note that a
  restore to a *different* path creates a fresh project entry and will re-prompt for trust.
