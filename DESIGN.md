# How ccvault works

Notes on the design, and on the undocumented Claude Code behaviour it is built around.
For installation and usage, see [README.md](README.md).

## Two findings that shaped everything

### Resume is not directory-locked

Claude Code's `/resume` picker only lists sessions whose recorded working directory matches
where you launched it, which makes it look as though a conversation belongs to a directory.
It does not. `claude --resume <session-id>` works from anywhere — verified by resuming a
session recorded under one directory from an unrelated temporary directory and getting the
full prior context back.

That inverted the problem. The missing capability was never *resumption*; it was **discovery**
and **reconstruction**. ccvault never reimplements resume — it finds the session, rebuilds
somewhere worth resuming into, and hands off to `claude --resume`.

### Git provenance is never recorded

Each transcript record carries the working directory, a bare branch name, the Claude Code
version and timestamps. There is no remote URL and no commit SHA — and for a directory that
is not a repository, the branch field simply reads `HEAD`.

So a deleted repository cannot be reconstructed from what Claude Code stores. That has a
hard consequence: **every session run before ccvault is installed has an unrecoverable
commit.** Capture had to be the first thing built, before search or restore, because it is
the only part with a deadline.

## The lossy-slug trap

Transcripts live in `~/.claude/projects/<slug-of-cwd>/<session-id>.jsonl`, where the slug is
the working directory with separators replaced. It is tempting to reverse it to recover the
path. **You cannot.** `/`, `.` and `_` all collapse to `-`:

```
~/code/a.b_c  ─┐
               ├─→  ~/.claude/projects/-home-u-code-a-b-c/   (one folder, two sessions)
~/code/a-b-c  ─┘
```

Verified empirically: sessions in two different directories landed in one project folder with
their transcripts interleaved. With ordinary repository naming this collides in practice —
`agent-os` versus `agent.os`, `my_repo` versus `my-repo`.

> **Invariant:** identity is the session UUID; location is the `cwd` field read from *inside*
> the transcript records. The folder name is an opaque shard and is never parsed.

`TestSlugCollisionKeepsSessionsDistinct` guards this.

### A session can span several directories

Because `--resume` works from anywhere and appends to the original transcript, one file can
contain records with several different working directories. ccvault keeps the first as the
session's home and records the rest.

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
                          transcripts/, patches/, snapshots/
                                     │
                    ccvault search ──┤── TUI picker
                    ccvault restore ─┴── rebuild → exec claude --resume
```

Capture is deliberately belt-and-braces. Hooks give exact git state at the moments it exists;
the timer catches everything hooks miss and picks up transcript growth. Unchanged transcripts
are skipped on a size comparison, which is reliable because they are append-only.

**The hook must never wedge a session.** It exits 0 unconditionally, wraps its work in a
timeout, and `SessionStart` runs asynchronously so it cannot delay startup. `SessionEnd` runs
synchronously — an async hook can lose the race with process exit — but is capped at 10
seconds.

## Two kinds of directory, two restore strategies

The split that matters is not repo-versus-not, it is **what can reconstruct this**:

| | Restored by | Why |
|---|---|---|
| Repository with a remote | clone + checkout + replay | remote and SHA reconstruct it; storing the tree would be redundant |
| Repository with no remote | tree archive **including `.git`** | nothing else holds its history |
| Plain directory | tree archive | no git safety net at all |

A clone-based restore replays three things on top: the dirty patch (`git diff --binary HEAD`),
the untracked file contents, and `.claude/` when the repository does not track it.

That last one is easy to miss. A project's `.claude/` directory holds its skills and agents.
If it is untracked, a clone silently returns without them and the restored session quietly
loses capabilities — so ccvault records whether it is tracked, and archives it separately when
it is not.

## Data model

```sql
sessions(uuid PRIMARY KEY, cwd, cwds, slug, title, first_prompt,
         started_at, last_active, kind, msg_count, cc_version, git_branch,
         bytes, sha256, dir_state, vault_path)

git_state(session_uuid, captured_at, event, worktree_root, remote_url, branch,
          head_sha, is_dirty, patch_path, untracked_path, submodules_json,
          claude_dir_tracked, …)

snapshots(session_uuid, captured_at, kind, path, bytes, sha256, file_count, …)

sessions_fts USING fts5(uuid, title, first_prompt, cwd, body)
```

`git_state` keeps one row per capture event rather than overwriting, so a restore can prefer
the most informative one — a dirty capture with a patch beats a clean one taken later.

Snapshot archives are **content-addressed**: named by a hash of the tree, so many sessions
sharing one working directory reference a single file instead of storing a copy each. A
shared archive is only deleted once nothing points at it.

## Search

FTS5 over titles, first prompts and the human-authored text of each session. Tool results are
excluded — they dominate by volume but are machine output, not signal.

The index is also seeded from `~/.claude/history.jsonl`, Claude Code's global prompt log.
Transcripts are pruned after `cleanupPeriodDays` (30 by default) while the prompt log persists,
so those sessions stay findable and are marked `NO TRANSCRIPT` rather than vanishing.

**Incremental search needs care.** Querying on every keystroke means half-typed input reaches
the FTS5 parser constantly, where a stray `"`, `(` or `-` is a syntax error rather than a
search — the picker would look broken on ordinary typing. Each token is quoted and given a
trailing `*` for prefix matching.

## Why settings live in a file

ccvault runs from three places with no shared environment: the CLI you type, a hook spawned
inside a Claude session, and the reconcile timer.

An exported `CCVAULT_HOME` reaches the hook, because it inherits your shell. It does **not**
reach the timer, which has no shell — a systemd user unit has no `Environment=` lines and the
variable is not in the user manager's environment. Verified: with the variable exported, the
hook wrote to the alternate vault while the timer kept using the default, producing two
half-vaults each holding part of the history.

A file is read identically by all three. The environment variables remain, for tests, and are
reported loudly when in effect.

## Removable vaults

Putting the vault on an external drive introduces a failure mode worth designing for: when
the drive is unmounted, the mountpoint is usually **an empty directory that still exists**. A
naive tool creates a fresh vault inside it, and on remount the real vault reappears
underneath — two incomplete stores, neither obviously wrong.

ccvault writes a sentinel file into the vault and records, in local state that survives the
drive going away, which filesystem the vault was on. That distinguishes:

- **unmounted** — the path exists but its device differs from the recorded one → reconnect
- **deleted** — same device, vault gone → nothing to reconnect; recreate empty
- **occupied** — something else lives there → refuse

and it refuses to create a vault in any of them without an explicit override.

### The holding area

A hook cannot prompt for a new location. Skipping would permanently lose the commit SHA and
the diff. So captures taken while the vault is unreachable are written to local state with
their git provenance intact, and move into the vault only when the user runs
`ccvault pending adopt`.

## Bugs that only appeared against real state

Both were silent, and both were found by restoring an actual directory rather than trusting
the tests:

**Every dirty patch was corrupt.** The git wrapper ran command output through `strings.TrimSpace`,
which strips the trailing newline. Git rejects such a patch outright with `corrupt patch`. The
entire dirty-state feature was inert from the first commit, and no unit test noticed because
the patch text looked fine. Patches now come from a raw variant that preserves bytes exactly,
and `WritePatch` guarantees a trailing newline regardless of caller.

**`restore <id> -n` performed a real restore.** Go's `flag` package stops parsing at the first
positional argument, so `-n` after the session id was read as another argument and never as a
flag. A preview that wrote to disk and then launched Claude. Arguments are now permuted so
flags are seen wherever they appear, with an explicit `--` terminator so a positional that
looks like a flag stays positional.

The general lesson: the interesting failures were at the seams with the outside world — git's
output format, the flag parser's conventions, the filesystem's behaviour when a drive
vanishes. Unit tests over our own code could not have caught any of them.

## Testing

```sh
go test ./...
```

Tests use synthetic fixtures and redirect both `CCVAULT_CLAUDE_HOME` and `CCVAULT_HOME`, so
they never touch a real vault. The restore tests build actual local git remotes and clone
from them, so the clone path is genuinely exercised rather than mocked. The launchd agent is
XML-parsed in tests so its structure is verified from Linux.
