package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sysn3rd/ccvault/internal/config"
	"github.com/sysn3rd/ccvault/internal/snapshot"
)

// field is one editable setting. Get and Set work in strings so the editor does
// not need a type per row; Set validates and reports why it refused.
type field struct {
	key   string
	label string
	help  string
	get   func(*config.Config) string
	set   func(*config.Config, string) error
}

func settingsFields() []field {
	num := func(name string, get func(*config.Config) *int) field {
		return field{
			key: name, label: name,
			get: func(c *config.Config) string { return strconv.Itoa(*get(c)) },
			set: func(c *config.Config, v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n <= 0 {
					return fmt.Errorf("%s needs a positive whole number", name)
				}
				*get(c) = n
				return nil
			},
		}
	}

	return []field{
		{
			key: "vault_dir", label: "vault_dir",
			help: "where backups live — an external drive is fine",
			get:  func(c *config.Config) string { return c.VaultDir },
			set: func(c *config.Config, v string) error {
				v = strings.TrimSpace(v)
				if v == "" {
					return fmt.Errorf("vault_dir cannot be empty")
				}
				c.VaultDir = expandHome(v)
				return nil
			},
		},
		{
			key: "claude_home", label: "claude_home",
			help: "Claude Code's own directory, read-only to ccvault",
			get:  func(c *config.Config) string { return c.ClaudeHome },
			set: func(c *config.Config, v string) error {
				v = strings.TrimSpace(v)
				if v == "" {
					return fmt.Errorf("claude_home cannot be empty")
				}
				c.ClaudeHome = expandHome(v)
				return nil
			},
		},
		func() field {
			f := num("max_file_mb", func(c *config.Config) *int { return &c.Snapshots.MaxFileMB })
			f.help = "skip single files larger than this when archiving"
			return f
		}(),
		func() field {
			f := num("max_total_mb", func(c *config.Config) *int { return &c.Snapshots.MaxTotalMB })
			f.help = "cap on one directory archive"
			return f
		}(),
		func() field {
			f := num("snapshots_kept", func(c *config.Config) *int { return &c.Retention.Snapshots })
			f.help = "archives retained per session"
			return f
		}(),
		func() field {
			f := num("git_states_kept", func(c *config.Config) *int { return &c.Retention.GitStates })
			f.help = "git captures retained per session"
			return f
		}(),
		{
			key: "ignore_dirs", label: "ignore_dirs",
			help: "directory names never archived, space separated",
			get: func(c *config.Config) string {
				if c.Snapshots.IgnoreDirs == nil {
					return strings.Join(snapshot.DefaultIgnoreDirs, " ")
				}
				return strings.Join(c.Snapshots.IgnoreDirs, " ")
			},
			set: func(c *config.Config, v string) error {
				c.Snapshots.IgnoreDirs = strings.Fields(v)
				return nil
			},
		},
		{
			key: "terminal", label: "terminal",
			help: "command for 'open in a new terminal' — blank auto-detects",
			get:  func(c *config.Config) string { return c.Picker.Terminal },
			set: func(c *config.Config, v string) error {
				c.Picker.Terminal = strings.TrimSpace(v)
				return nil
			},
		},
	}
}

func (m *model) openSettings() {
	cfg, err := config.Load()
	if err != nil {
		m.status = "could not read settings: " + err.Error()
		return
	}
	m.cfg = cfg
	m.fields = settingsFields()
	m.fieldCursor = 0
	m.editing = false
	m.dirty = false
	m.mode = modeSettings
	m.status = ""
}

func (m *model) updateSettings(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.editing {
		switch msg.String() {
		case "esc":
			m.editing = false
			m.status = "edit cancelled"
		case "enter":
			f := m.fields[m.fieldCursor]
			if err := f.set(m.cfg, m.editBuf); err != nil {
				m.status = err.Error()
				return m, nil
			}
			m.editing = false
			m.dirty = true
			m.status = f.key + " changed — ^s to save"
		case "backspace":
			if m.editBuf != "" {
				m.editBuf = m.editBuf[:len(m.editBuf)-1]
			}
		case "ctrl+u":
			m.editBuf = ""
		default:
			if len(msg.Runes) > 0 {
				m.editBuf += string(msg.Runes)
			}
		}
		return m, nil
	}

	switch msg.String() {
	case "esc", "q":
		if m.dirty {
			// Losing an edit silently would be worse than one extra keystroke.
			m.status = "unsaved changes — ^s to save, or esc again to discard"
			m.dirty = false
			return m, nil
		}
		m.mode = modeList
		m.status = ""
	case "ctrl+c":
		return m, tea.Quit
	case "up", "ctrl+p", "ctrl+k":
		m.fieldCursor = clamp(m.fieldCursor-1, 0, len(m.fields)-1)
	case "down", "ctrl+n", "ctrl+j":
		m.fieldCursor = clamp(m.fieldCursor+1, 0, len(m.fields)-1)
	case "enter":
		m.editing = true
		m.editBuf = m.fields[m.fieldCursor].get(m.cfg)
		m.status = ""
	case "ctrl+s":
		if err := m.cfg.Save(); err != nil {
			m.status = "could not save: " + err.Error()
			return m, nil
		}
		m.dirty = false
		st := m.cfg.Check()
		if st.State == config.VaultOK {
			m.status = "saved to " + m.cfg.Path
		} else {
			// Saying this here is the difference between "I moved my backups"
			// and "my backups quietly stopped happening".
			m.status = "saved — but " + st.Detail
		}
	}
	return m, nil
}

func (m *model) renderSettings(width int) string {
	var b stringBuilder
	b.line(styleSelected.Render("Settings"))
	b.line(styleDim.Render(truncate(m.cfg.Path, width)))
	b.line("")

	// Truncate the value before any styling is applied: ANSI codes would
	// otherwise be counted as visible width and the box would overflow.
	const labelWidth = 16
	valueWidth := max(8, width-labelWidth-3)

	for i, f := range m.fields {
		marker := "  "
		if i == m.fieldCursor {
			marker = "› "
		}
		value := f.get(m.cfg)
		if value == "" {
			value = "(unset)"
		}
		if m.editing && i == m.fieldCursor {
			// Keep the caret visible by showing the tail of a long value.
			value = tailOf(m.editBuf, valueWidth-1) + "▊"
		} else {
			value = truncate(value, valueWidth)
		}
		head := fmt.Sprintf("%s%-*s ", marker, labelWidth, f.label)
		if i == m.fieldCursor {
			head = styleSelected.Render(head)
		}
		b.line(head + value)
		if i == m.fieldCursor && f.help != "" {
			b.line(styleDim.Render(truncate("    "+f.help, width)))
		}
	}

	st := m.cfg.Check()
	b.line("")
	if st.State == config.VaultOK {
		b.line(styleDim.Render("vault ok"))
	} else {
		b.line(styleWarn.Render(truncate(string(st.State)+": "+st.Detail, width)))
	}
	return b.String()
}

// tailOf keeps the end of a string, which is where the cursor is while typing.
func tailOf(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n+1:])
}

// expandHome resolves a leading ~ so the editor accepts what a person types.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}
