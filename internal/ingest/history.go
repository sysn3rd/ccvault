package ingest

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/index"
)

// historyEntry is one line of ~/.claude/history.jsonl — Claude Code's global
// prompt log, written for every prompt in every directory.
type historyEntry struct {
	Display   string `json:"display"`
	Timestamp int64  `json:"timestamp"` // milliseconds
	Project   string `json:"project"`
	SessionID string `json:"sessionId"`
}

// SeedFromHistory indexes sessions that appear in the prompt log but have no
// transcript on disk.
//
// Claude Code prunes transcripts after cleanupPeriodDays (30 by default) while
// history.jsonl keeps the prompt text indefinitely. Those sessions are gone as
// resumable context, but they are exactly the ones a person searches for months
// later — "what was that thing I did with the audiobook?" — so they belong in
// the index, clearly marked as having no transcript.
//
// Sessions we already hold a transcript for are left untouched: that record is
// strictly richer.
func SeedFromHistory(cfg *config.Config, db *index.DB) (int, error) {
	f, err := os.Open(cfg.HistoryFile())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	// Only the set of ids is needed here; List would join every session against
	// its latest git capture to build the same set.
	known := map[string]bool{}
	ids, err := db.UUIDs()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		known[id] = true
	}

	type agg struct {
		project string
		prompts []string
		firstTS int64
		lastTS  int64
	}
	bySession := map[string]*agg{}
	order := []string{}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e historyEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // tolerant: a bad line is not worth failing the scan
		}
		if e.SessionID == "" || known[e.SessionID] {
			continue
		}
		a := bySession[e.SessionID]
		if a == nil {
			a = &agg{project: e.Project, firstTS: e.Timestamp, lastTS: e.Timestamp}
			bySession[e.SessionID] = a
			order = append(order, e.SessionID)
		}
		if txt := strings.TrimSpace(e.Display); txt != "" {
			a.prompts = append(a.prompts, txt)
		}
		if e.Timestamp < a.firstTS || a.firstTS == 0 {
			a.firstTS = e.Timestamp
		}
		if e.Timestamp > a.lastTS {
			a.lastTS = e.Timestamp
		}
		if a.project == "" {
			a.project = e.Project
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}

	sort.Strings(order)
	seeded := 0
	for _, uuid := range order {
		a := bySession[uuid]
		if len(a.prompts) == 0 {
			continue
		}
		s := &index.Session{
			UUID:        uuid,
			CWD:         a.project,
			CWDs:        []string{a.project},
			FirstPrompt: truncate(a.prompts[0], 500),
			StartedAt:   msToTime(a.firstTS),
			LastActive:  msToTime(a.lastTS),
			MsgCount:    len(a.prompts),
			Kind:        index.KindPlain,
			DirState:    index.DirMissing,
			// VaultPath stays empty: that is what marks this as prompt-log only.
		}
		if dirExists(a.project) {
			s.DirState = index.DirOK
			if exists(a.project + "/.git") {
				s.Kind = index.KindRepo
			}
		}
		if err := db.UpsertSession(s, strings.Join(a.prompts, "\n")); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

func msToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
