# Security policy

## Reporting a vulnerability

Please report security issues privately, via GitHub's
[private vulnerability reporting](https://github.com/sysn3rd/ccvault/security/advisories/new)
rather than a public issue.

Include what you were doing, what happened, and a reproduction if you have one. You will get
an acknowledgement; this is a personal project, so please allow a little time.

## What is in scope

ccvault handles unusually sensitive data and executes external commands, so the interesting
areas are:

- **Anything reaching `exec`.** Remote URLs, branch names and commits are captured from
  whatever `.git/config` was in a directory you opened — they are not trusted input. Git
  parses an argument beginning with `-` as an option, so a remote of `--upload-pack=<command>`
  is command execution. These are validated in `internal/restore/validate.go`; a bypass is a
  vulnerability.
- **Archive extraction.** Restore unpacks tar archives. Path traversal, symlink and hardlink
  entries, and absolute paths are rejected — a bypass is a vulnerability.
- **File permissions.** The vault holds full transcripts: file contents, command output,
  anything you pasted. Everything under it is `0600`, directories `0700`. Anything that
  widens those is a vulnerability.
- **The capture hook.** It runs inside a live Claude session. A way to make it hang, crash a
  session, or write outside the vault is in scope.

## What is not in scope

- **The vault is not encrypted at rest.** This is a known limitation, documented in the
  README, not a vulnerability. Treat the vault as being exactly as sensitive as the sessions
  it holds.
- **No redaction of secrets inside transcripts.** ccvault stores what Claude Code stored.
- **Someone with read access to your account can read your vault.** File permissions protect
  against other users, not against yourself.
