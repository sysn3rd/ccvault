package tui

import "testing"

func TestBuildPrefixQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"godot", `"godot"*`},
		{"fun game", `"fun"* "game"*`},
		// Punctuation is what breaks a naive FTS query; it must be stripped or quoted.
		{"a-b_c", `"a"* "b"* "c"*`},
		{`say "hi"`, `"say"* "hi"*`},
		{"^caret NEAR(", `"caret"* "NEAR"*`},
		{"...", ""},
		{"claude/code", `"claude"* "code"*`},
	}
	for _, c := range cases {
		if got := BuildPrefixQuery(c.in); got != c.want {
			t.Errorf("BuildPrefixQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
