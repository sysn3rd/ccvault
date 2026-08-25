// Package index is the SQLite catalogue of every session ccvault knows about.
//
// Identity is the session UUID. Location is the cwd read from inside the
// transcript — never the project folder name, which is lossy: Claude Code maps
// /, . and _ all to -, so a.b_c and a-b-c share one folder. See VISION.md.
package index

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct{ sql *sql.DB }

// Kind distinguishes the two restore strategies.
const (
	KindRepo  = "repo"  // rebuild by cloning remote at a SHA
	KindPlain = "plain" // rebuild by unpacking a tree snapshot
)

// Directory state as of the last scan.
const (
	DirOK      = "ok"
	DirMissing = "missing"
	// DirEmpty is a directory that still exists but has nothing in it. Resuming
	// into one lands you somewhere useless, so it is distinguished from DirOK
	// and is restorable in place without --force.
	DirEmpty = "empty"
)

// Snapshot kinds.
const (
	SnapTree   = "tree"   // the whole working directory
	SnapClaude = "claude" // just .claude/, when the repo does not track it
)

type Session struct {
	UUID        string
	CWD         string
	CWDs        []string
	Slug        string
	Title       string
	FirstPrompt string
	StartedAt   time.Time
	LastActive  time.Time
	Kind        string
	MsgCount    int
	CCVersion   string
	GitBranch   string
	Bytes       int64
	SHA256      string
	DirState    string
	VaultPath   string

	// Latest git provenance, joined in for display. Empty for plain directories.
	Branch    string
	HeadSHA   string
	RemoteURL string
}

// HasTranscript reports whether ccvault holds a copy of the conversation.
// It is false for sessions known only from ~/.claude/history.jsonl — Claude Code
// prunes transcripts after cleanupPeriodDays (30 by default), but the prompt log
// keeps the text, so the session stays searchable even once the context is gone.
func (s *Session) HasTranscript() bool { return s.VaultPath != "" }

// StateLabel is the single source of truth for how a session's health renders.
// The picker and the plain listing both use it so they cannot drift apart.
func (s *Session) StateLabel() string {
	switch {
	case !s.HasTranscript():
		return "NO TRANSCRIPT"
	case s.DirState == DirMissing:
		return "MISSING"
	case s.DirState == DirEmpty:
		return "EMPTY"
	default:
		return "OK"
	}
}

// NeedsRestore reports whether the directory must be rebuilt before Claude can
// usefully be launched into it.
func (s *Session) NeedsRestore() bool {
	return s.DirState == DirMissing || s.DirState == DirEmpty
}

// GitRef renders "branch@abcd1234" for display, or "" when there is no git state.
func (s *Session) GitRef() string {
	switch {
	case s.Branch == "" && s.HeadSHA == "":
		return ""
	case s.HeadSHA == "":
		return s.Branch
	case len(s.HeadSHA) >= 8:
		return s.Branch + "@" + s.HeadSHA[:8]
	default:
		return s.Branch + "@" + s.HeadSHA
	}
}

type GitState struct {
	ID               int64
	SessionUUID      string
	CapturedAt       time.Time
	Event            string // "start" | "end" | "scan"
	WorktreeRoot     string
	RemoteURL        string
	Branch           string
	HeadSHA          string
	IsDirty          bool
	PatchPath        string
	UntrackedJSON    string
	UntrackedPath    string
	UntrackedSkipped string
	SubmodulesJSON   string
	ClaudeDirTracked bool
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
  uuid          TEXT PRIMARY KEY,
  cwd           TEXT NOT NULL,
  cwds          TEXT NOT NULL DEFAULT '[]',
  slug          TEXT,
  title         TEXT,
  first_prompt  TEXT,
  started_at    INTEGER,
  last_active   INTEGER,
  kind          TEXT,
  msg_count     INTEGER,
  cc_version    TEXT,
  git_branch    TEXT,
  bytes         INTEGER,
  sha256        TEXT,
  dir_state     TEXT,
  vault_path    TEXT,
  updated_at    INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_cwd  ON sessions(cwd);
CREATE INDEX IF NOT EXISTS sessions_last ON sessions(last_active DESC);

CREATE TABLE IF NOT EXISTS git_state (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  session_uuid       TEXT NOT NULL,
  captured_at        INTEGER NOT NULL,
  event              TEXT,
  worktree_root      TEXT,
  remote_url         TEXT,
  branch             TEXT,
  head_sha           TEXT,
  is_dirty           INTEGER,
  patch_path         TEXT,
  untracked_json     TEXT,
  submodules_json    TEXT,
  claude_dir_tracked INTEGER
);
CREATE INDEX IF NOT EXISTS git_state_session ON git_state(session_uuid, captured_at DESC);

CREATE TABLE IF NOT EXISTS snapshots (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_uuid TEXT NOT NULL,
  captured_at  INTEGER NOT NULL,
  kind         TEXT,
  path         TEXT,
  bytes        INTEGER,
  sha256       TEXT
);
CREATE INDEX IF NOT EXISTS snapshots_session ON snapshots(session_uuid, captured_at DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS sessions_fts USING fts5(
  uuid UNINDEXED, title, first_prompt, cwd, body
);
`

var migrations = []string{
	`ALTER TABLE git_state ADD COLUMN untracked_path TEXT`,
	`ALTER TABLE git_state ADD COLUMN untracked_skipped_json TEXT`,
	`ALTER TABLE snapshots ADD COLUMN skipped_json TEXT`,
	`ALTER TABLE snapshots ADD COLUMN file_count INTEGER`,
	`ALTER TABLE snapshots ADD COLUMN include_git INTEGER`,
	`ALTER TABLE sessions ADD COLUMN snapshot_checked_at INTEGER`,
}

func Open(path string) (*DB, error) {
	// WAL keeps the hook's write from blocking a concurrent scan, and busy_timeout
	// means neither gives up if they do collide.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, err
	}
	// Columns added after the first release. SQLite has no IF NOT EXISTS for
	// ALTER TABLE, so a duplicate-column error is the expected no-op.
	for _, stmt := range migrations {
		if _, err := d.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			d.Close()
			return nil, err
		}
	}

	// SQLite creates these honoring umask, which is typically 0644. The index
	// holds prompt text and the full-text body, so tighten it to owner-only —
	// the same standard the transcript copies are held to.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			d.Close()
			return nil, err
		}
	}
	return &DB{sql: d}, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// UpsertSession writes session metadata and refreshes its FTS row.
// body is the concatenated human text; it is stored only in the FTS index.
func (db *DB) UpsertSession(s *Session, body string) error {
	tx, err := db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	cwds, _ := json.Marshal(s.CWDs)
	_, err = tx.Exec(`
    INSERT INTO sessions (uuid,cwd,cwds,slug,title,first_prompt,started_at,last_active,
                          kind,msg_count,cc_version,git_branch,bytes,sha256,dir_state,vault_path,updated_at)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(uuid) DO UPDATE SET
      cwd=excluded.cwd, cwds=excluded.cwds, slug=excluded.slug, title=excluded.title,
      first_prompt=excluded.first_prompt, started_at=excluded.started_at,
      last_active=excluded.last_active, kind=excluded.kind, msg_count=excluded.msg_count,
      cc_version=excluded.cc_version, git_branch=excluded.git_branch, bytes=excluded.bytes,
      sha256=excluded.sha256, dir_state=excluded.dir_state, vault_path=excluded.vault_path,
      updated_at=excluded.updated_at`,
		s.UUID, s.CWD, string(cwds), s.Slug, s.Title, s.FirstPrompt,
		unix(s.StartedAt), unix(s.LastActive), s.Kind, s.MsgCount, s.CCVersion,
		s.GitBranch, s.Bytes, s.SHA256, s.DirState, s.VaultPath, time.Now().Unix())
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM sessions_fts WHERE uuid = ?`, s.UUID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO sessions_fts (uuid,title,first_prompt,cwd,body) VALUES (?,?,?,?,?)`,
		s.UUID, s.Title, s.FirstPrompt, strings.Join(s.CWDs, " "), body); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) InsertGitState(g *GitState) error {
	_, err := db.sql.Exec(`
    INSERT INTO git_state (session_uuid,captured_at,event,worktree_root,remote_url,branch,
                           head_sha,is_dirty,patch_path,untracked_json,submodules_json,
                           claude_dir_tracked,untracked_path,untracked_skipped_json)
    VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.SessionUUID, unix(g.CapturedAt), g.Event, g.WorktreeRoot, g.RemoteURL, g.Branch,
		g.HeadSHA, g.IsDirty, g.PatchPath, g.UntrackedJSON, g.SubmodulesJSON, g.ClaudeDirTracked,
		g.UntrackedPath, g.UntrackedSkipped)
	return err
}

// KnownSHA reports the transcript hash we last ingested, so scan can skip files
// that have not grown. Transcripts are append-only, so size alone would do, but
// the hash also catches a file being replaced wholesale.
func (db *DB) KnownTranscript(uuid string) (bytes int64, sha string, ok bool) {
	row := db.sql.QueryRow(`SELECT bytes, sha256 FROM sessions WHERE uuid = ?`, uuid)
	if err := row.Scan(&bytes, &sha); err != nil {
		return 0, "", false
	}
	return bytes, sha, true
}

// Get returns one session by UUID, or nil when unknown.
func (db *DB) Get(uuid string) (*Session, error) {
	rows, err := db.sql.Query(selectSessions+` WHERE s.uuid = ?`, uuid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanSessions(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// MarkSnapshotChecked records that the archiving decision has been made for a
// session — including a decision that it needs no archive at all, which is the
// normal answer for a repository whose remote already covers it. Without this,
// every reconcile would re-evaluate those sessions forever, shelling out to git
// each time.
func (db *DB) MarkSnapshotChecked(uuid string) error {
	_, err := db.sql.Exec(`UPDATE sessions SET snapshot_checked_at = ? WHERE uuid = ?`,
		time.Now().Unix(), uuid)
	return err
}

// SnapshotChecked reports whether that decision has already been made.
func (db *DB) SnapshotChecked(uuid string) bool {
	var at int64
	if err := db.sql.QueryRow(
		`SELECT COALESCE(snapshot_checked_at,0) FROM sessions WHERE uuid = ?`, uuid).Scan(&at); err != nil {
		return false
	}
	return at > 0
}

func (db *DB) SetDirState(uuid, state string) error {
	_, err := db.sql.Exec(`UPDATE sessions SET dir_state = ? WHERE uuid = ?`, state, uuid)
	return err
}

// selectSessions joins each session to its most recent git capture. A session
// with no git_state rows still comes back, with empty git columns.
const selectSessions = `
  SELECT s.uuid,s.cwd,s.cwds,s.slug,s.title,s.first_prompt,s.started_at,s.last_active,
         s.kind,s.msg_count,s.cc_version,s.git_branch,s.bytes,s.sha256,s.dir_state,s.vault_path,
         COALESCE(g.branch,''),COALESCE(g.head_sha,''),COALESCE(g.remote_url,'')
  FROM sessions s
  LEFT JOIN (
    SELECT session_uuid, branch, head_sha, remote_url,
           ROW_NUMBER() OVER (PARTITION BY session_uuid ORDER BY captured_at DESC) AS rn
    FROM git_state
  ) g ON g.session_uuid = s.uuid AND g.rn = 1`

func (db *DB) List() ([]*Session, error) {
	rows, err := db.sql.Query(selectSessions + ` ORDER BY s.last_active DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// Search runs an FTS5 MATCH. The query is passed through, so FTS syntax works;
// a bare word behaves as a prefix-free term match.
func (db *DB) Search(q string) ([]*Session, error) {
	rows, err := db.sql.Query(selectSessions+`
      JOIN sessions_fts f ON f.uuid = s.uuid
      WHERE sessions_fts MATCH ? ORDER BY bm25(sessions_fts), s.last_active DESC`, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSessions(rows)
}

// LatestGitState returns the most recent git capture for a session, preferring
// the richest one: a dirty capture with a patch beats a clean one taken later.
func (db *DB) LatestGitState(uuid string) (*GitState, error) {
	row := db.sql.QueryRow(`
      SELECT session_uuid,captured_at,event,worktree_root,remote_url,branch,head_sha,
             is_dirty,patch_path,untracked_json,submodules_json,claude_dir_tracked,
             COALESCE(untracked_path,''),COALESCE(untracked_skipped_json,'')
      FROM git_state WHERE session_uuid = ? ORDER BY captured_at DESC LIMIT 1`, uuid)
	g := &GitState{}
	var ts int64
	err := row.Scan(&g.SessionUUID, &ts, &g.Event, &g.WorktreeRoot, &g.RemoteURL, &g.Branch,
		&g.HeadSHA, &g.IsDirty, &g.PatchPath, &g.UntrackedJSON, &g.SubmodulesJSON, &g.ClaudeDirTracked,
		&g.UntrackedPath, &g.UntrackedSkipped)
	if err != nil {
		return nil, err
	}
	g.CapturedAt = time.Unix(ts, 0)
	return g, nil
}

type Snapshot struct {
	ID          int64
	SessionUUID string
	CapturedAt  time.Time
	Kind        string
	Path        string
	Bytes       int64
	// TreeHash identifies the directory contents; an unchanged directory
	// produces the same hash, so a repeat snapshot can be skipped.
	TreeHash    string
	FileCount   int
	IncludeGit  bool
	SkippedJSON string
}

func (db *DB) InsertSnapshot(s *Snapshot) error {
	_, err := db.sql.Exec(`
    INSERT INTO snapshots (session_uuid,captured_at,kind,path,bytes,sha256,
                           skipped_json,file_count,include_git)
    VALUES (?,?,?,?,?,?,?,?,?)`,
		s.SessionUUID, unix(s.CapturedAt), s.Kind, s.Path, s.Bytes, s.TreeHash,
		s.SkippedJSON, s.FileCount, s.IncludeGit)
	return err
}

// LatestSnapshot returns the newest snapshot of a kind, or nil when there is none.
func (db *DB) LatestSnapshot(uuid, kind string) (*Snapshot, error) {
	rows, err := db.sql.Query(`
      SELECT id,session_uuid,captured_at,kind,path,bytes,COALESCE(sha256,''),
             COALESCE(skipped_json,''),COALESCE(file_count,0),COALESCE(include_git,0)
      FROM snapshots WHERE session_uuid = ? AND kind = ?
      ORDER BY captured_at DESC LIMIT 1`, uuid, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanSnapshots(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// StaleSnapshots returns snapshots beyond the newest `keep` for a kind, so the
// caller can delete their files and rows. Without this a directory that changes
// between every session accumulates one archive per session forever.
func (db *DB) StaleSnapshots(uuid, kind string, keep int) ([]*Snapshot, error) {
	rows, err := db.sql.Query(`
      SELECT id,session_uuid,captured_at,kind,path,bytes,COALESCE(sha256,''),
             COALESCE(skipped_json,''),COALESCE(file_count,0),COALESCE(include_git,0)
      FROM snapshots WHERE session_uuid = ? AND kind = ?
      ORDER BY captured_at DESC LIMIT -1 OFFSET ?`, uuid, kind, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

// CountSnapshotRefs reports how many rows still point at an archive. Snapshot
// files are content-addressed and therefore shared, so one must never be
// deleted while another session still references it.
func (db *DB) CountSnapshotRefs(path string) (int, error) {
	var n int
	err := db.sql.QueryRow(`SELECT COUNT(*) FROM snapshots WHERE path = ?`, path).Scan(&n)
	return n, err
}

func (db *DB) DeleteSnapshot(id int64) error {
	_, err := db.sql.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

func scanSnapshots(rows *sql.Rows) ([]*Snapshot, error) {
	var out []*Snapshot
	for rows.Next() {
		s := &Snapshot{}
		var ts int64
		if err := rows.Scan(&s.ID, &s.SessionUUID, &ts, &s.Kind, &s.Path, &s.Bytes,
			&s.TreeHash, &s.SkippedJSON, &s.FileCount, &s.IncludeGit); err != nil {
			return nil, err
		}
		s.CapturedAt = time.Unix(ts, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReferencedFiles is every vault path any row still points at. Anything in the
// vault outside this set is an orphan and safe to delete.
func (db *DB) ReferencedFiles() (map[string]bool, error) {
	refs := map[string]bool{}
	queries := []string{
		`SELECT vault_path FROM sessions WHERE vault_path != ''`,
		`SELECT patch_path FROM git_state WHERE patch_path IS NOT NULL AND patch_path != ''`,
		`SELECT untracked_path FROM git_state WHERE untracked_path IS NOT NULL AND untracked_path != ''`,
		`SELECT path FROM snapshots WHERE path != ''`,
	}
	for _, q := range queries {
		rows, err := db.sql.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			refs[p] = true
		}
		rows.Close()
	}
	return refs, nil
}

// StaleGitStates returns git captures beyond the newest `keep` for a session.
// Every capture event may carry a patch and an untracked bundle, so without
// pruning these grow once per session start and end, forever.
func (db *DB) StaleGitStates(uuid string, keep int, before time.Time) ([]*GitState, error) {
	q := `
      SELECT id,session_uuid,captured_at,COALESCE(patch_path,''),COALESCE(untracked_path,'')
      FROM git_state WHERE session_uuid = ?
      ORDER BY captured_at DESC LIMIT -1 OFFSET ?`
	rows, err := db.sql.Query(q, uuid, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*GitState
	for rows.Next() {
		g := &GitState{}
		var ts int64
		if err := rows.Scan(&g.ID, &g.SessionUUID, &ts, &g.PatchPath, &g.UntrackedPath); err != nil {
			return nil, err
		}
		g.CapturedAt = time.Unix(ts, 0)
		if !before.IsZero() && g.CapturedAt.After(before) {
			continue
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (db *DB) DeleteGitState(id int64) error {
	_, err := db.sql.Exec(`DELETE FROM git_state WHERE id = ?`, id)
	return err
}

// UUIDs lists every indexed session id.
func (db *DB) UUIDs() ([]string, error) {
	rows, err := db.sql.Query(`SELECT uuid FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Vacuum reclaims space after bulk deletions.
func (db *DB) Vacuum() error {
	_, err := db.sql.Exec(`VACUUM`)
	return err
}

// Forget removes a session and everything stored for it, returning the vault
// files the caller should delete. It touches only ccvault's own records —
// ~/.claude is never modified.
func (db *DB) Forget(uuid string) ([]string, error) {
	var files []string
	if s, err := db.Get(uuid); err == nil && s != nil && s.VaultPath != "" {
		files = append(files, s.VaultPath)
	}
	rows, err := db.sql.Query(`
      SELECT COALESCE(patch_path,''), COALESCE(untracked_path,'')
      FROM git_state WHERE session_uuid = ?`, uuid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var patch, untracked string
		if err := rows.Scan(&patch, &untracked); err != nil {
			rows.Close()
			return nil, err
		}
		for _, f := range []string{patch, untracked} {
			if f != "" {
				files = append(files, f)
			}
		}
	}
	rows.Close()

	// Snapshot archives are content-addressed and shared between sessions, so
	// only those with no other referrer are safe to delete.
	snapRows, err := db.sql.Query(`
      SELECT path FROM snapshots WHERE session_uuid = ? AND path != ''
        AND (SELECT COUNT(*) FROM snapshots o WHERE o.path = snapshots.path
             AND o.session_uuid != ?) = 0`, uuid, uuid)
	if err != nil {
		return nil, err
	}
	for snapRows.Next() {
		var p string
		if err := snapRows.Scan(&p); err != nil {
			snapRows.Close()
			return nil, err
		}
		files = append(files, p)
	}
	snapRows.Close()

	tx, err := db.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM sessions WHERE uuid = ?`,
		`DELETE FROM sessions_fts WHERE uuid = ?`,
		`DELETE FROM git_state WHERE session_uuid = ?`,
		`DELETE FROM snapshots WHERE session_uuid = ?`,
	} {
		if _, err := tx.Exec(stmt, uuid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return files, nil
}

type StatsResult struct {
	Sessions      int
	Missing       int
	Empty         int
	NoTranscript  int
	TranscriptB   int64
	SnapshotB     int64
	SnapshotFiles int
}

func (db *DB) Stats() (*StatsResult, error) {
	r := &StatsResult{}
	if err := db.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM sessions`).
		Scan(&r.Sessions, &r.TranscriptB); err != nil {
		return nil, err
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sessions WHERE dir_state = ?`, DirMissing).
		Scan(&r.Missing); err != nil {
		return nil, err
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sessions WHERE dir_state = ?`, DirEmpty).
		Scan(&r.Empty); err != nil {
		return nil, err
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sessions WHERE vault_path = ''`).
		Scan(&r.NoTranscript); err != nil {
		return nil, err
	}
	// Distinct paths: archives are shared, so summing rows would double-count.
	if err := db.sql.QueryRow(`
      SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM (
        SELECT path, MAX(bytes) AS bytes FROM snapshots GROUP BY path)`).
		Scan(&r.SnapshotFiles, &r.SnapshotB); err != nil {
		return nil, err
	}
	return r, nil
}

func scanSessions(rows *sql.Rows) ([]*Session, error) {
	var out []*Session
	for rows.Next() {
		s := &Session{}
		var started, last int64
		var cwds string
		if err := rows.Scan(&s.UUID, &s.CWD, &cwds, &s.Slug, &s.Title, &s.FirstPrompt,
			&started, &last, &s.Kind, &s.MsgCount, &s.CCVersion, &s.GitBranch,
			&s.Bytes, &s.SHA256, &s.DirState, &s.VaultPath,
			&s.Branch, &s.HeadSHA, &s.RemoteURL); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(cwds), &s.CWDs)
		s.StartedAt = time.Unix(started, 0)
		s.LastActive = time.Unix(last, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
