package main

import (
	"cmp"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type help struct {
	input    textinput.Model
	rows     []string
	filtered []string
	cursor   int
}

func newHelp(bindings map[string]string, bookmarks []Bookmark, commands []Command) *help {
	keysByAction := map[string][]string{}
	for key, action := range bindings {
		keysByAction[action] = append(keysByAction[action], key)
	}
	row := func(action, desc string) string {
		keys := keysByAction[action]
		sort.Strings(keys)
		return fmt.Sprintf("%-22s %s", strings.Join(keys, " "), desc)
	}

	var rows []string
	for _, a := range actions {
		if len(keysByAction[a.name]) > 0 {
			rows = append(rows, row(a.name, a.desc))
		}
	}
	for i, c := range commands {
		rows = append(rows, row(fmt.Sprintf("cmd:%d", i), cmp.Or(c.Desc, c.Run)))
	}
	for i, b := range bookmarks {
		if b.Key != "" {
			rows = append(rows, row(fmt.Sprintf("bookmark:%d", i), "Go to "+b.Name))
		}
	}
	rows = append(rows,
		fmt.Sprintf("%-22s %s", "f  z", "Finder: tab switch mode, ↑↓ / ctrl+j ctrl+k move, enter open"),
		fmt.Sprintf("%-22s %s", "1-9", "Go to tab"),
		fmt.Sprintf("%-22s %s", "esc", "Cancel visual / clear filter / clear selection"),
		fmt.Sprintf("%-22s %s", "ctrl+c", "Quit without cd"),
	)

	input := textinput.New()
	input.Prompt = "❯ "
	input.Placeholder = "filter…"
	input.Focus()
	return &help{input: input, rows: rows, filtered: rows}
}

func (m *model) updateHelp(msg tea.KeyMsg) tea.Cmd {
	h := m.help
	switch msg.String() {
	case "esc":
		m.help = nil
	case "up", "ctrl+p", "ctrl+k":
		h.cursor = clamp(h.cursor-1, 0, len(h.filtered)-1)
	case "down", "ctrl+n", "ctrl+j":
		h.cursor = clamp(h.cursor+1, 0, len(h.filtered)-1)
	default:
		var cmd tea.Cmd
		h.input, cmd = h.input.Update(msg)
		h.filter()
		return cmd
	}
	return nil
}

func (h *help) filter() {
	query := strings.ToLower(h.input.Value())
	h.filtered = h.filtered[:0:0]
	for _, r := range h.rows {
		if strings.Contains(strings.ToLower(r), query) {
			h.filtered = append(h.filtered, r)
		}
	}
	h.cursor = clamp(h.cursor, 0, len(h.filtered)-1)
}

func (h *help) view(w, height int) string {
	title := accentStyle.Render(" Keybindings ") + dimStyle.Render("  esc close")
	footer := fmt.Sprintf("%d/%d", len(h.filtered), len(h.rows))
	return overlayView(title, &h.input, h.filtered, h.cursor, "", false, footer, w, height)
}
