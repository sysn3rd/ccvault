package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sysn3rd/ccvault/internal/index"
)

func testDB(t *testing.T) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	add := func(uuid, title, cwd, kind, dirState, vaultPath, body string) {
		s := &index.Session{
			UUID: uuid, CWD: cwd, CWDs: []string{cwd}, Title: title,
			FirstPrompt: body, Kind: kind, DirState: dirState,
			VaultPath: vaultPath, LastActive: time.Now().Add(-time.Hour),
		}
		if err := db.UpsertSession(s, body); err != nil {
			t.Fatal(err)
		}
	}
	add("11111111-0000-0000-0000-000000000000", "Fun game ideas", "/home/u/code/games",
		index.KindPlain, index.DirMissing, "/vault/1.jsonl", "godot roguelike ideas")
	add("22222222-0000-0000-0000-000000000000", "Phase 3 task groups", "/home/u/code/TheLastAssembly",
		index.KindRepo, index.DirOK, "/vault/2.jsonl", "task group completion")
	add("33333333-0000-0000-0000-000000000000", "Audiobook from USB", "/home/u/Work",
		index.KindPlain, index.DirOK, "", "playing an audiobook") // pruned: no transcript
	return db
}

func typeString(t *testing.T, m *model, s string) {
	t.Helper()
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func key(t *testing.T, m *model, k tea.KeyType) tea.Model {
	t.Helper()
	next, _ := m.Update(tea.KeyMsg{Type: k})
	return next
}

func TestPickerListsEverythingBeforeTyping(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.results) != 3 {
		t.Fatalf("got %d results with an empty box, want 3", len(m.results))
	}
}

func TestPickerFiltersIncrementally(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}

	// Mid-word: prefix matching has to work on every keystroke.
	typeString(t, m, "godo")
	if len(m.results) != 1 {
		t.Fatalf("typing 'godo' gave %d results, want 1", len(m.results))
	}
	if m.results[0].Title != "Fun game ideas" {
		t.Errorf("matched %q", m.results[0].Title)
	}

	key(t, m, tea.KeyCtrlU)
	if m.input != "" || len(m.results) != 3 {
		t.Errorf("ctrl+u did not reset (input=%q results=%d)", m.input, len(m.results))
	}
}

// Punctuation reaches the FTS parser raw; a syntax error there would look like
// "no results" to the user, or crash the picker.
func TestPickerSurvivesPunctuation(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"`, `^`, `NEAR(`, `a-b`, `*`, `()`} {
		m.input = ""
		typeString(t, m, s)
		if m.err != nil {
			t.Fatalf("input %q produced an error: %v", s, m.err)
		}
	}
}

func TestEnterResumesLiveSession(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "task")
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionResume {
		t.Fatalf("Action = %v, want ActionResume", m.result.Action)
	}
	if m.result.Session.UUID != "22222222-0000-0000-0000-000000000000" {
		t.Errorf("resumed the wrong session: %s", m.result.Session.UUID)
	}
}

// A deleted directory has to be rebuilt before Claude can be launched into it,
// so Enter means restore rather than resume.
func TestEnterOnMissingDirectoryRestores(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "godot")
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionRestore {
		t.Errorf("Action = %v, want ActionRestore for a missing directory", m.result.Action)
	}
	if m.result.Session.UUID != "11111111-0000-0000-0000-000000000000" {
		t.Errorf("selected the wrong session: %s", m.result.Session.UUID)
	}
}

// An empty directory resumes into somewhere useless, so it restores too.
func TestEnterOnEmptyDirectoryRestores(t *testing.T) {
	db := testDB(t)
	if err := db.UpsertSession(&index.Session{
		UUID: "44444444-0000-0000-0000-000000000000", CWD: "/home/u/code/emptied",
		CWDs: []string{"/home/u/code/emptied"}, Title: "Emptied out",
		Kind: index.KindPlain, DirState: index.DirEmpty, VaultPath: "/vault/4.jsonl",
		LastActive: time.Now(),
	}, "emptied session"); err != nil {
		t.Fatal(err)
	}

	m, err := New(db, "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "emptied")
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionRestore {
		t.Errorf("Action = %v, want ActionRestore for an empty directory", m.result.Action)
	}
}

// The footer must name what Enter will actually do for the highlighted row.
func TestFooterNamesTheAction(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 30

	typeString(t, m, "godot") // missing directory
	if !strings.Contains(m.View(), "enter restore") {
		t.Error("footer should offer restore for a missing directory")
	}

	key(t, m, tea.KeyCtrlU)
	typeString(t, m, "task") // live directory
	if !strings.Contains(m.View(), "enter resume") {
		t.Error("footer should offer resume for a live directory")
	}
}

// A pruned session has prompts but no context; resuming it is meaningless.
func TestEnterOnPrunedSessionExplains(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "audiobook")
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionNone {
		t.Errorf("Action = %v, want ActionNone for a pruned session", m.result.Action)
	}
	if !strings.Contains(m.status, "prompt log only") {
		t.Errorf("status = %q", m.status)
	}
}

func TestNavigationClamps(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		key(t, m, tea.KeyDown)
	}
	if m.cursor != len(m.results)-1 {
		t.Errorf("cursor = %d, want %d", m.cursor, len(m.results)-1)
	}
	for i := 0; i < 10; i++ {
		key(t, m, tea.KeyUp)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d after scrolling up, want 0", m.cursor)
	}
}

func TestViewShowsStateMarkers(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 30
	view := m.View()

	for _, want := range []string{"MISSING", "NO TRANSCRIPT", "Fun game ideas", "^o open dir"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q", want)
		}
	}
}

func TestViewHandlesEmptyResults(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "zzzzznotathing")
	if len(m.results) != 0 {
		t.Fatalf("expected no results, got %d", len(m.results))
	}
	if !strings.Contains(m.View(), "no matches") {
		t.Error("view should say 'no matches'")
	}
	// Enter with nothing selected must be a no-op, not a panic.
	key(t, m, tea.KeyEnter)
	if m.result.Action != ActionNone {
		t.Error("Enter on an empty list should do nothing")
	}
}
