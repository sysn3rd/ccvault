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

func (db *DB) Stats() (sessions, missing int, bytes int64, err error) {
	err = db.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(bytes),0) FROM sessions`).Scan(&sessions, &bytes)
	if err != nil {
		return
	}
	err = db.sql.QueryRow(`SELECT COUNT(*) FROM sessions WHERE dir_state = ?`, DirMissing).Scan(&missing)
	return
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
