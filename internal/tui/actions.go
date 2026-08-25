package tui

import (
	"fmt"

	"github.com/sysn3rd/ccvault/internal/index"
)

// actionKind is what a menu entry does when chosen.
type actionKind int

const (
	actContinueHere actionKind = iota
	actNewTerminal
	actRestore
	actOpenDir
	actCopyID
)

// menuItem is one row of the action menu. A disabled row is shown with the
// reason rather than hidden, so it is obvious what would make it possible.
type menuItem struct {
	kind    actionKind
	label   string
	hint    string
	enabled bool
	why     string
}

// actionsFor builds the menu for a session. The ordering puts the thing you
// most likely want first, which changes depending on whether the directory is
// still there.
func actionsFor(s *index.Session) []menuItem {
	needsRestore := s.NeedsRestore()
	hasTranscript := s.HasTranscript()

	// One reason covers both resume entries, and it is the reason the user
	// cares about: the directory has to exist before Claude can be launched in it.
	var resumeWhy string
	switch {
	case needsRestore && s.DirState == index.DirMissing:
		resumeWhy = "the directory no longer exists — restore it first"
	case needsRestore:
		resumeWhy = "the directory is empty — restore it first"
	case !hasTranscript:
		resumeWhy = "Claude Code pruned this transcript; there is no conversation left to resume"
	}
	canResume := resumeWhy == ""

	items := []menuItem{
		{
			kind: actNewTerminal, label: "Open in a new terminal",
			hint: "leaves this picker open", enabled: canResume, why: resumeWhy,
		},
		{
			kind: actContinueHere, label: "Continue in this window",
			hint: "replaces the picker", enabled: canResume, why: resumeWhy,
		},
	}

	restoreHint := "rebuild the directory, then resume"
	items = append(items, menuItem{
		kind: actRestore, label: "Restore the directory", hint: restoreHint,
		enabled: needsRestore,
		why:     ternary(needsRestore, "", "the directory is already there"),
	})

	items = append(items, menuItem{
		kind: actOpenDir, label: "Open the directory", hint: "in your file manager",
		enabled: !needsRestore,
		why:     ternary(needsRestore, "there is nothing to open yet", ""),
	})
	items = append(items, menuItem{
		kind: actCopyID, label: "Copy the session id", enabled: true,
	})

	// Restoring is the only useful thing for a session whose directory is gone,
	// so it leads.
	if needsRestore {
		reordered := []menuItem{items[2]}
		reordered = append(reordered, items[0], items[1], items[3], items[4])
		return reordered
	}
	return items
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// firstEnabled is where the cursor starts, so Enter twice does the sensible
// thing rather than landing on a disabled row.
func firstEnabled(items []menuItem) int {
	for i, it := range items {
		if it.enabled {
			return i
		}
	}
	return 0
}

func (m *model) renderActions(width int) string {
	s := m.selected()
	if s == nil {
		return ""
	}
	var b stringBuilder
	b.line(styleSelected.Render(truncate(displayTitle(s), width)))
	b.line(styleDim.Render(truncate(collapseHome(s.CWD)+"  ·  "+s.StateLabel(), width)))
	b.line("")

	for i, it := range m.actions {
		marker := "  "
		if i == m.actionCursor {
			marker = "› "
		}
		label := it.label
		if !it.enabled {
			label += "  (unavailable)"
		}
		// Compose from pre-truncated parts; styling must never affect width.
		head := truncate(marker+label, width)
		if i == m.actionCursor && it.enabled {
			head = styleSelected.Render(head)
		}
		line := head
		if it.enabled && it.hint != "" {
			room := width - runeLen(marker+label) - 4
			if room > 6 {
				line += "  " + styleDim.Render(truncate("— "+it.hint, room))
			}
		}
		b.line(line)
		if !it.enabled && it.why != "" {
			b.line(styleDim.Render(truncate("      "+it.why, width)))
		}
	}
	return b.String()
}

// stringBuilder keeps the rendering readable without repeating newline handling.
type stringBuilder struct{ parts []string }

func (b *stringBuilder) line(s string) { b.parts = append(b.parts, s) }
func (b *stringBuilder) String() string {
	out := ""
	for i, p := range b.parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

func describeSelection(s *index.Session) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%s · %s", collapseHome(s.CWD), s.StateLabel())
}
