package tui

import (
	"strings"
	"unicode"
)

// BuildPrefixQuery turns whatever the user has typed so far into a safe FTS5
// query.
//
// Incremental search means querying on every keystroke, mid-word — so each term
// becomes a prefix match. It also means arbitrary input reaches the FTS parser,
// where a stray quote, hyphen or caret is a syntax error rather than a search.
// Quoting each token defuses that: FTS5 treats "..." as a literal string, and a
// trailing * outside the quotes makes it a prefix.
//
// Returns "" when there is nothing searchable, meaning "show everything".
func BuildPrefixQuery(input string) string {
	tokens := strings.FieldsFunc(input, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(tokens) == 0 {
		return ""
	}
	var parts []string
	for _, t := range tokens {
		// A quote cannot survive inside a quoted FTS string; doubling escapes it.
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	return strings.Join(parts, " ")
}
