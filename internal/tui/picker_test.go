package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sysn3rd/ccvault/internal/config"
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

// selectAction moves the menu cursor to a given action, so tests do not depend
// on menu ordering.
func selectAction(t *testing.T, m *model, kind actionKind) {
	t.Helper()
	for i, it := range m.actions {
		if it.kind == kind {
			m.actionCursor = i
			return
		}
	}
	t.Fatalf("action %v is not in the menu", kind)
}

// Enter no longer guesses what you meant; it shows the choices.
func TestEnterOpensTheActionMenu(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "task")
	key(t, m, tea.KeyEnter)

	if m.mode != modeActions {
		t.Fatal("Enter should open the action menu")
	}
	if m.result.Action != ActionNone {
		t.Errorf("nothing should have happened yet, got %v", m.result.Action)
	}
	if len(m.actions) == 0 {
		t.Fatal("menu is empty")
	}
	if !m.actions[m.actionCursor].enabled {
		t.Error("the cursor should start on an enabled action")
	}
}

func TestMenuOpensInANewTerminal(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "task")
	key(t, m, tea.KeyEnter)
	selectAction(t, m, actNewTerminal)
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionResumeNewWindow {
		t.Fatalf("Action = %v, want ActionResumeNewWindow", m.result.Action)
	}
	if m.result.Session.UUID != "22222222-0000-0000-0000-000000000000" {
		t.Errorf("wrong session: %s", m.result.Session.UUID)
	}
}

func TestMenuContinuesInPlace(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "task")
	key(t, m, tea.KeyEnter)
	selectAction(t, m, actContinueHere)
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionResume {
		t.Fatalf("Action = %v, want ActionResume", m.result.Action)
	}
}

// The directory has to exist before Claude can be launched in it, so both
// resume options stay disabled — with the reason visible — until it is restored.
func TestMenuGatesResumeUntilRestored(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "godot") // directory is MISSING
	key(t, m, tea.KeyEnter)

	byKind := map[actionKind]menuItem{}
	for _, it := range m.actions {
		byKind[it.kind] = it
	}
	for _, k := range []actionKind{actNewTerminal, actContinueHere} {
		if byKind[k].enabled {
			t.Errorf("action %v should be disabled while the directory is gone", k)
		}
		if !strings.Contains(byKind[k].why, "restore it first") {
			t.Errorf("action %v reason = %q, want it to point at restoring", k, byKind[k].why)
		}
	}
	if !byKind[actRestore].enabled {
		t.Error("restore should be the enabled option here")
	}
	// Restoring is the only useful thing, so it should lead.
	if m.actions[0].kind != actRestore {
		t.Errorf("first action = %v, want restore", m.actions[0].kind)
	}
	if m.actions[m.actionCursor].kind != actRestore {
		t.Error("the cursor should start on restore")
	}

	key(t, m, tea.KeyEnter)
	if m.result.Action != ActionRestore {
		t.Errorf("Action = %v, want ActionRestore", m.result.Action)
	}
}

// A pruned transcript cannot be resumed at all, and the menu should say why
// rather than offering something that would fail.
func TestMenuExplainsPrunedTranscript(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "audiobook")
	key(t, m, tea.KeyEnter)

	for _, it := range m.actions {
		if it.kind != actContinueHere && it.kind != actNewTerminal {
			continue
		}
		if it.enabled {
			t.Errorf("action %v should be disabled for a pruned session", it.kind)
		}
		if !strings.Contains(it.why, "pruned") {
			t.Errorf("reason = %q, want it to mention pruning", it.why)
		}
	}
}

// Choosing a disabled row must explain, not act.
func TestMenuRefusesDisabledAction(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "godot")
	key(t, m, tea.KeyEnter)
	selectAction(t, m, actContinueHere)
	key(t, m, tea.KeyEnter)

	if m.result.Action != ActionNone {
		t.Errorf("a disabled action must not run, got %v", m.result.Action)
	}
	if m.status == "" {
		t.Error("choosing a disabled action should explain why")
	}
}

func TestEscapeLeavesTheMenu(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	typeString(t, m, "task")
	key(t, m, tea.KeyEnter)
	if m.mode != modeActions {
		t.Fatal("expected to be in the menu")
	}
	key(t, m, tea.KeyEsc)
	if m.mode != modeList {
		t.Error("esc should return to the list")
	}
	if m.result.Action != ActionNone {
		t.Error("esc should not run anything")
	}
}

// An unavailable vault must never look like lost work.
func TestPendingBannerIsShown(t *testing.T) {
	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 30
	m.SetPending(3)
	view := m.View()
	if !strings.Contains(view, "3 capture(s) held") {
		t.Error("the pending banner is missing")
	}
	if !strings.Contains(view, "pending adopt") {
		t.Error("the banner should say how to file them")
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

// The settings editor exists so relocating the vault does not require finding
// and hand-editing a TOML file.
func TestSettingsEditorOpensAndEdits(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CCVAULT_HOME", "")
	t.Setenv("CCVAULT_CONFIG", "")

	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.width, m.height = 100, 34

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{','}})
	if m.mode != modeSettings {
		t.Fatal("',' should open the settings editor")
	}
	if !strings.Contains(m.View(), "vault_dir") {
		t.Error("settings view does not show vault_dir")
	}

	// Edit vault_dir to somewhere an external drive might live.
	selectField(t, m, "vault_dir")
	key(t, m, tea.KeyEnter)
	if !m.editing {
		t.Fatal("Enter should start editing the field")
	}
	key(t, m, tea.KeyCtrlU)
	typeString(t, m, "/mnt/backup/ccvault")
	key(t, m, tea.KeyEnter)

	if m.editing {
		t.Error("Enter should commit the edit")
	}
	if m.cfg.VaultDir != "/mnt/backup/ccvault" {
		t.Errorf("VaultDir = %q", m.cfg.VaultDir)
	}
	if !m.dirty {
		t.Error("an edit should mark the settings unsaved")
	}

	// Save, then confirm it round-trips through the file.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if m.dirty {
		t.Error("saving should clear the unsaved marker")
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.VaultDir != "/mnt/backup/ccvault" {
		t.Errorf("reloaded VaultDir = %q, want the edited value", reloaded.VaultDir)
	}
}

// A rejected value must not be written, and must say why.
func TestSettingsEditorRejectsBadInput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CCVAULT_HOME", "")
	t.Setenv("CCVAULT_CONFIG", "")

	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{','}})
	selectField(t, m, "max_file_mb")
	before := m.cfg.Snapshots.MaxFileMB

	key(t, m, tea.KeyEnter)
	key(t, m, tea.KeyCtrlU)
	typeString(t, m, "not-a-number")
	key(t, m, tea.KeyEnter)

	if m.cfg.Snapshots.MaxFileMB != before {
		t.Errorf("MaxFileMB changed to %d despite invalid input", m.cfg.Snapshots.MaxFileMB)
	}
	if !m.editing {
		t.Error("a rejected edit should stay in edit mode so it can be fixed")
	}
	if !strings.Contains(m.status, "positive whole number") {
		t.Errorf("status = %q, want an explanation", m.status)
	}
}

// Discarding an edit by accident would be worse than one extra keystroke.
func TestSettingsWarnsBeforeDiscarding(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CCVAULT_HOME", "")
	t.Setenv("CCVAULT_CONFIG", "")

	m, err := New(testDB(t), "")
	if err != nil {
		t.Fatal(err)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{','}})
	selectField(t, m, "terminal")
	key(t, m, tea.KeyEnter)
	typeString(t, m, "foot")
	key(t, m, tea.KeyEnter)

	key(t, m, tea.KeyEsc)
	if m.mode != modeSettings {
		t.Error("the first esc with unsaved changes should warn, not leave")
	}
	if !strings.Contains(m.status, "unsaved") {
		t.Errorf("status = %q, want an unsaved-changes warning", m.status)
	}
	key(t, m, tea.KeyEsc)
	if m.mode != modeList {
		t.Error("a second esc should leave")
	}
}

func selectField(t *testing.T, m *model, key string) {
	t.Helper()
	for i, f := range m.fields {
		if f.key == key {
			m.fieldCursor = i
			return
		}
	}
	t.Fatalf("field %q not found", key)
}
