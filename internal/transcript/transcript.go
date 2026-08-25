// Package transcript reads Claude Code's session JSONL files.
//
// The format is undocumented and versioned per-record (every record carries the
// Claude Code version that wrote it). The parser is therefore deliberately
// tolerant: unknown record types are skipped, unparseable lines are counted and
// ignored, and no field is required except the session id.
package transcript

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Session is the distilled metadata for one conversation.
type Session struct {
	UUID string

	// CWD is the directory the session was *first* launched in. This is the
	// one that matters for restore.
	CWD string
	// CWDs is every distinct directory the session has run in, in first-seen
	// order. A transcript really can span several: `claude --resume <uuid>`
	// works from any directory and appends to the original file, so resuming
	// from elsewhere interleaves a second cwd into the same transcript.
	CWDs []string

	Title       string // from ai-title records, if Claude titled the session
	FirstPrompt string
	StartedAt   time.Time
	LastActive  time.Time
	MsgCount    int // human turns, not tool traffic
	Version     string
	GitBranch   string

	// Body is the concatenated human-authored text, used to seed FTS.
	// Tool results and images are excluded — they are bulk, not signal.
	Body string

	Bytes    int64
	SHA256   string
	BadLines int
}

// record is the tolerant superset of every record type we care about.
type record struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	UUID      string `json:"uuid"`
	CWD       string `json:"cwd"`
	Version   string `json:"version"`
	GitBranch string `json:"gitBranch"`
	Timestamp string `json:"timestamp"`
	AITitle   string `json:"aiTitle"`
	IsMeta    bool   `json:"isMeta"`
	Sidechain bool   `json:"isSidechain"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// humanText pulls the operator-authored text out of a user message.
// Content is either a bare string or an array of blocks; in the array form the
// vast majority of blocks are tool_result payloads, which we drop.
func humanText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

var (
	commandNameRE = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	// Tags Claude Code wraps around injected content. None of it is text the
	// operator typed, so none of it belongs in the search index.
	strippedTagRE = regexp.MustCompile(`(?s)<(system-reminder|local-command-stdout|command-message|command-args)>.*?</(system-reminder|local-command-stdout|command-message|command-args)>`)
)

// slashCommand recognises the record Claude Code writes when a slash command is
// invoked. It reports the command as typed ("/init") so that is what gets
// indexed, rather than the XML wrapper.
func slashCommand(text string) (string, bool) {
	m := commandNameRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	name := strings.TrimSpace(m[1])
	if name == "" {
		return "", false
	}
	return name, true
}

// Parse reads one transcript file end to end.
func Parse(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	sum := sha256.New()
	sc := bufio.NewScanner(io.TeeReader(f, sum))
	// Individual records embed whole file contents and command output, so lines
	// run far past the default 64KB limit.
	sc.Buffer(make([]byte, 0, 256*1024), 32*1024*1024)

	s := &Session{Bytes: st.Size()}
	var body strings.Builder
	seenCWD := map[string]bool{}
	// A slash command is written as two consecutive user records: the invocation
	// and then the command's expanded prompt text. The expansion is boilerplate
	// the operator never wrote, so it is dropped rather than indexed.
	skipExpansion := false

	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var r record
		if err := json.Unmarshal(line, &r); err != nil {
			s.BadLines++
			continue
		}

		if s.UUID == "" && r.SessionID != "" {
			s.UUID = r.SessionID
		}
		if r.CWD != "" {
			if s.CWD == "" {
				s.CWD = r.CWD
			}
			if !seenCWD[r.CWD] {
				seenCWD[r.CWD] = true
				s.CWDs = append(s.CWDs, r.CWD)
			}
		}
		if r.Version != "" {
			s.Version = r.Version
		}
		if r.GitBranch != "" && s.GitBranch == "" {
			s.GitBranch = r.GitBranch
		}
		if r.Type == "ai-title" && r.AITitle != "" {
			s.Title = r.AITitle // later titles supersede earlier ones
		}
		if ts := parseTime(r.Timestamp); !ts.IsZero() {
			if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
				s.StartedAt = ts
			}
			if ts.After(s.LastActive) {
				s.LastActive = ts
			}
		}

		if r.Type == "user" && !r.IsMeta && !r.Sidechain && r.Message != nil {
			txt := humanText(r.Message.Content)
			if name, isCmd := slashCommand(txt); isCmd {
				txt = name
				skipExpansion = true
			} else if skipExpansion {
				skipExpansion = false
				txt = ""
			}
			txt = strings.TrimSpace(strippedTagRE.ReplaceAllString(txt, ""))
			if txt != "" {
				s.MsgCount++
				if s.FirstPrompt == "" {
					s.FirstPrompt = truncate(txt, 500)
				}
				body.WriteString(txt)
				body.WriteByte('\n')
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	s.Body = body.String()
	s.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return s, nil
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
