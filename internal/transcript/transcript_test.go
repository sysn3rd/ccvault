package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTranscript builds a JSONL fixture from raw record lines.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.jsonl")
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseExtractsMetadata(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"mode","mode":"normal","sessionId":"abc"}`,
		`{"type":"user","sessionId":"abc","cwd":"/home/u/Work","version":"2.1.240","gitBranch":"main","timestamp":"2026-08-01T10:00:00.000Z","message":{"role":"user","content":"how do I fix the brightness"}}`,
		`{"type":"ai-title","aiTitle":"Brightness fix","sessionId":"abc"}`,
		`{"type":"user","sessionId":"abc","cwd":"/home/u/Work","timestamp":"2026-08-01T10:05:00.000Z","message":{"role":"user","content":[{"type":"text","text":"second turn"}]}}`,
	)

	s, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.UUID != "abc" {
		t.Errorf("UUID = %q, want abc", s.UUID)
	}
	if s.CWD != "/home/u/Work" {
		t.Errorf("CWD = %q", s.CWD)
	}
	if s.Title != "Brightness fix" {
		t.Errorf("Title = %q", s.Title)
	}
	if s.FirstPrompt != "how do I fix the brightness" {
		t.Errorf("FirstPrompt = %q", s.FirstPrompt)
	}
	if s.MsgCount != 2 {
		t.Errorf("MsgCount = %d, want 2", s.MsgCount)
	}
	if s.Version != "2.1.240" || s.GitBranch != "main" {
		t.Errorf("version/branch = %q/%q", s.Version, s.GitBranch)
	}
	if s.StartedAt.After(s.LastActive) {
		t.Error("StartedAt should not be after LastActive")
	}
}

// A resumed session appends records carrying a different cwd, because
// `claude --resume <uuid>` runs from anywhere and writes to the original file.
func TestParseRecordsEveryCWD(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","sessionId":"abc","cwd":"/home/u/Work","message":{"role":"user","content":"first"}}`,
		`{"type":"user","sessionId":"abc","cwd":"/tmp/elsewhere","message":{"role":"user","content":"resumed from a different directory"}}`,
		`{"type":"user","sessionId":"abc","cwd":"/home/u/Work","message":{"role":"user","content":"back again"}}`,
	)

	s, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.CWD != "/home/u/Work" {
		t.Errorf("CWD should be the first directory seen, got %q", s.CWD)
	}
	want := []string{"/home/u/Work", "/tmp/elsewhere"}
	if len(s.CWDs) != len(want) {
		t.Fatalf("CWDs = %v, want %v", s.CWDs, want)
	}
	for i := range want {
		if s.CWDs[i] != want[i] {
			t.Errorf("CWDs[%d] = %q, want %q", i, s.CWDs[i], want[i])
		}
	}
}

// Tool results dominate user records by volume but are machine output, not
// anything the operator typed.
func TestParseExcludesToolResults(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":[{"type":"tool_result","content":"secret-token-abc123"}]}}`,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":[{"type":"text","text":"real question"}]}}`,
	)

	s, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.MsgCount != 1 {
		t.Errorf("MsgCount = %d, want 1 (tool_result must not count)", s.MsgCount)
	}
	if s.FirstPrompt != "real question" {
		t.Errorf("FirstPrompt = %q", s.FirstPrompt)
	}
	if got := s.Body; contains(got, "secret-token") {
		t.Error("tool_result content leaked into the indexed body")
	}
}

// /init writes the invocation and then the command's expanded prompt. Only the
// invocation is something the operator typed.
func TestParseCollapsesSlashCommands(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"<command-message>init</command-message>\n<command-name>/init</command-name>"}}`,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"Please analyze this codebase and create a CLAUDE.md file, which will be given to future instances"}}`,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"now add a test"}}`,
	)

	s, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.FirstPrompt != "/init" {
		t.Errorf("FirstPrompt = %q, want /init", s.FirstPrompt)
	}
	if contains(s.Body, "Please analyze this codebase") {
		t.Error("slash-command expansion was indexed; it is boilerplate, not user text")
	}
	if !contains(s.Body, "now add a test") {
		t.Error("the turn after an expansion must still be indexed")
	}
	if s.MsgCount != 2 {
		t.Errorf("MsgCount = %d, want 2 (/init + the real turn)", s.MsgCount)
	}
}

// The format is undocumented and version-stamped per record; a bad line must
// never abort the parse.
func TestParseToleratesGarbage(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"good"}}`,
		`this is not json at all`,
		`{"type":"some-future-record-type","sessionId":"abc","whatever":{"nested":true}}`,
		``,
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"also good"}}`,
	)

	s, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.MsgCount != 2 {
		t.Errorf("MsgCount = %d, want 2", s.MsgCount)
	}
	if s.BadLines != 1 {
		t.Errorf("BadLines = %d, want 1", s.BadLines)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
