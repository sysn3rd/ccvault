// Package tui is the interactive session picker.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sysn3rd/ccvault/internal/index"
)

// Action is what the caller should do once the picker exits.
type Action int

const (
	ActionNone Action = iota
	ActionResume
	// ActionRestore rebuilds the directory first. The picker only offers it when
	// the directory is gone or empty.
	ActionRestore
)

type Result struct {
	Action  Action
	Session *index.Session
}

var (
	styleBorder   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	stylePrompt   = lipgloss.NewStyle().Bold(true)
	styleSelected = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleWarn     = lipgloss.NewStyle().Bold(true)
	styleHelp     = lipgloss.NewStyle().Faint(true)
)

type model struct {
	db      *index.DB
	input   string
	results []*index.Session
	cursor  int
	top     int // first visible row, for scrolling
	width   int
	height  int
	status  string
	err     error
	result  Result
}

// New builds the picker, seeded with an initial query.
func New(db *index.DB, initial string) (*model, error) {
	m := &model{db: db, input: initial, width: 80, height: 24}
	if err := m.refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

// Run shows the picker and reports what the user chose.
func Run(db *index.DB, initial string) (Result, error) {
	m, err := New(db, initial)
	if err != nil {
		return Result{}, err
	}
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return Result{}, err
	}
	fm, ok := final.(*model)
	if !ok {
		return Result{}, nil
	}
	return fm.result, fm.err
}

func (m *model) Init() tea.Cmd { return nil }

// refresh re-runs the query. An empty box lists everything, newest first, which
// makes the picker useful before a single key is typed.
func (m *model) refresh() error {
	q := BuildPrefixQuery(m.input)
	var (
		rows []*index.Session
		err  error
	)
	if q == "" {
		rows, err = m.db.List()
	} else {
		rows, err = m.db.Search(q)
	}
	if err != nil {
		return err
	}
	m.results = rows
	if m.cursor >= len(rows) {
		m.cursor = max(0, len(rows)-1)
	}
	m.top = 0
	return nil
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit

		case "up", "ctrl+p", "ctrl+k":
			m.move(-1)
		case "down", "ctrl+n", "ctrl+j":
			m.move(1)
		case "pgup":
			m.move(-m.visibleRows())
		case "pgdown":
			m.move(m.visibleRows())

		case "enter":
			return m.choose()

		case "ctrl+o":
			m.openDir()
		case "ctrl+y":
			m.copyID()

		case "backspace":
			if m.input != "" {
				m.input = m.input[:len(m.input)-1]
				m.status = ""
				if err := m.refresh(); err != nil {
					m.err = err
					return m, tea.Quit
				}
			}
		case "ctrl+u":
			m.input = ""
			m.status = ""
			if err := m.refresh(); err != nil {
				m.err = err
				return m, tea.Quit
			}

		default:
			if len(msg.Runes) > 0 {
				m.input += string(msg.Runes)
				m.status = ""
				if err := m.refresh(); err != nil {
					m.err = err
					return m, tea.Quit
				}
			}
		}
	}
	return m, nil
}

func (m *model) move(delta int) {
	if len(m.results) == 0 {
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(m.results)-1)
	rows := m.visibleRows()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+rows {
		m.top = m.cursor - rows + 1
	}
}

// choose acts on Enter. Restore is a later phase, so the two cases it cannot
// handle are reported plainly rather than failing at exec time.
func (m *model) choose() (tea.Model, tea.Cmd) {
	s := m.selected()
	if s == nil {
		return m, nil
	}
	if !s.HasTranscript() {
		m.status = "prompt log only — Claude Code pruned this transcript, there is no context to resume"
		return m, nil
	}
	if s.NeedsRestore() {
		m.result = Result{Action: ActionRestore, Session: s}
		return m, tea.Quit
	}
	m.result = Result{Action: ActionResume, Session: s}
	return m, tea.Quit
}

func (m *model) openDir() {
	s := m.selected()
	if s == nil {
		return
	}
	if s.DirState == index.DirMissing {
		m.status = "directory is gone; nothing to open"
		return
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, s.CWD).Start(); err != nil {
		m.status = "could not open: " + err.Error()
		return
	}
	m.status = "opened " + collapseHome(s.CWD)
}

// copyID puts the session UUID on the clipboard, falling back to showing it so
// the user can select it by hand rather than getting silence.
func (m *model) copyID() {
	s := m.selected()
	if s == nil {
		return
	}
	candidates := [][]string{{"wl-copy"}, {"xclip", "-selection", "clipboard"}, {"pbcopy"}}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(s.UUID)
		if err := cmd.Run(); err == nil {
			m.status = "copied " + s.UUID
			return
		}
	}
	m.status = s.UUID
}

func (m *model) selected() *index.Session {
	if m.cursor < 0 || m.cursor >= len(m.results) {
		return nil
	}
	return m.results[m.cursor]
}

// visibleRows is how many sessions fit; each takes two lines, and the search
// box, borders, help line and status account for the rest.
func (m *model) visibleRows() int {
	const chrome = 8
	rows := (m.height - chrome) / 2
	return max(1, rows)
}

func (m *model) View() string {
	// Usable text columns inside the box: the rounded border costs 2 and the
	// horizontal padding another 2. Getting this wrong wraps every row.
	content := max(20, m.width-6)

	var b strings.Builder
	b.WriteString(stylePrompt.Render("> ") + m.input + "▊\n")
	b.WriteString(styleDim.Render(strings.Repeat("─", content)) + "\n")

	if len(m.results) == 0 {
		b.WriteString(styleDim.Render("no matches"))
	}

	rows := m.visibleRows()
	end := min(m.top+rows, len(m.results))
	for i := m.top; i < end; i++ {
		b.WriteString(m.renderRow(m.results[i], i == m.cursor, content))
	}

	// No explicit Width: the box sizes to its widest line, which is exactly
	// `content` because every row is padded to it.
	body := styleBorder.Render(strings.TrimRight(b.String(), "\n"))

	action := "resume"
	if s := m.selected(); s != nil && s.NeedsRestore() {
		action = "restore"
	}
	footer := styleHelp.Render(" enter " + action + " · ^o open dir · ^y copy id · ^u clear · esc quit")
	count := styleDim.Render(fmt.Sprintf(" %d session(s)", len(m.results)))
	if m.top+rows < len(m.results) {
		count = styleDim.Render(fmt.Sprintf(" %d session(s) · showing %d-%d", len(m.results), m.top+1, end))
	}

	out := body + "\n" + count + "\n" + footer
	if m.status != "" {
		out += "\n" + styleWarn.Render(" "+m.status)
	}
	return out
}

func (m *model) renderRow(s *index.Session, selected bool, width int) string {
	marker := "  "
	if selected {
		marker = "● "
	}

	// The path gets at most a third of the row; the title takes the rest.
	pathText := truncateMiddle(collapseHome(s.CWD), max(16, width/3))
	pathWidth := runeLen(pathText)
	titleWidth := max(10, width-pathWidth-runeLen(marker)-2)
	title := truncate(displayTitle(s), titleWidth)

	// Pad by rune count, not bytes: titles routinely contain non-ASCII.
	line1 := marker + pad(title, titleWidth) + "  " + pathText
	line1 = pad(truncate(line1, width), width)
	if selected {
		line1 = styleSelected.Render(line1)
	}

	meta := []string{relTime(s.LastActive), s.Kind}
	if ref := s.GitRef(); ref != "" {
		meta[1] = s.Kind + " " + ref
	}
	meta = append(meta, s.StateLabel())
	line2 := pad(truncate("    "+strings.Join(meta, " · "), width), width)

	return line1 + "\n" + styleDim.Render(line2) + "\n"
}

func displayTitle(s *index.Session) string {
	v := s.Title
	if v == "" {
		v = s.FirstPrompt
	}
	if v == "" {
		v = "(no prompts)"
	}
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = v[:i]
	}
	return v
}

func collapseHome(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(p, home) {
		return p
	}
	return "~" + strings.TrimPrefix(p, home)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// truncateMiddle keeps both ends of a path: the leading ~/code and the final
// directory name are the parts that identify it.
func truncateMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n || n < 5 {
		return truncate(s, n)
	}
	keep := n - 1
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

func runeLen(s string) int { return len([]rune(s)) }

// pad right-pads to n columns, counting runes rather than bytes.
func pad(s string, n int) string {
	if d := n - runeLen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// SetSize is used by rendering harnesses and tests that do not go through
// bubbletea's own window-size message.
func (m *model) SetSize(w, h int) { m.width, m.height = w, h }
