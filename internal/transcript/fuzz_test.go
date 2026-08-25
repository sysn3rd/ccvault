package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzParse exercises the transcript reader against arbitrary input.
//
// The format is undocumented, written by another program, and changes between
// Claude Code releases — exactly the shape of input a parser should be fuzzed
// against. Parse is required to be total: any bytes at all must produce either
// a session or an error, never a panic, because it runs inside a capture hook
// where a crash would surface as a broken Claude session.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{"type":"user","sessionId":"abc","cwd":"/w","message":{"role":"user","content":"hello"}}`,
		`{"type":"ai-title","aiTitle":"A title","sessionId":"abc"}`,
		`{"type":"user","sessionId":"abc","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"user","sessionId":"abc","message":{"role":"user","content":[{"type":"tool_result","content":"out"}]}}`,
		`{"type":"user","sessionId":"a","message":{"role":"user","content":"<command-name>/init</command-name>"}}`,
		`{"type":"future-record-type","nested":{"deeply":{"unknown":true}}}`,
		`{"type":"user","sessionId":"a","timestamp":"not-a-timestamp","message":{"role":"user","content":"x"}}`,
		"not json at all\n\n{\"type\":\"user\"}",
		"",
		// Content shapes the parser has to survive rather than trust.
		`{"type":"user","sessionId":"a","message":{"role":"user","content":{"unexpected":"object"}}}`,
		`{"type":"user","sessionId":"a","message":{"role":"user","content":[null,123,"str"]}}`,
		// A NUL inside a path, written as an escape rather than a raw byte.
		"{\"type\":\"user\",\"sessionId\":\"a\",\"cwd\":\"/w\\u0000x\",\"message\":{\"role\":\"user\",\"content\":\"x\"}}",
		// A record claiming a huge message count, to make sure nothing is trusted.
		`{"type":"user","sessionId":"a","msgCount":999999999,"message":{"role":"user","content":"x"}}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, body string) {
		path := filepath.Join(dir, "fuzz.jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Skip()
		}
		s, err := Parse(path)
		if err != nil {
			return // a refusal is a fine outcome; a panic is not
		}
		if s == nil {
			t.Fatal("Parse returned no error and no session")
		}

		// Invariants that must hold for whatever came back.
		if s.MsgCount < 0 || s.BadLines < 0 {
			t.Fatalf("negative counts: msgs=%d bad=%d", s.MsgCount, s.BadLines)
		}
		if s.CWD == "" && len(s.CWDs) > 0 {
			t.Fatal("CWDs recorded without a primary CWD")
		}
		for _, c := range s.CWDs {
			if c == "" {
				t.Fatal("an empty string was recorded as a working directory")
			}
		}
		if !s.StartedAt.IsZero() && !s.LastActive.IsZero() && s.StartedAt.After(s.LastActive) {
			t.Fatalf("StartedAt %v is after LastActive %v", s.StartedAt, s.LastActive)
		}
		// The first recorded directory is the session's home; it must be the
		// same value reported as CWD, or restore would target the wrong place.
		if len(s.CWDs) > 0 && s.CWDs[0] != s.CWD {
			t.Fatalf("CWD %q is not the first recorded directory %q", s.CWD, s.CWDs[0])
		}
	})
}
