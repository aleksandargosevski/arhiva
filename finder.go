package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"
)

const (
	modeLocal = iota
	modeZoxide
)

// ponytail: walk is capped and re-filtered on every keystroke; move to incremental/streaming matching if big trees feel slow.
const maxFinderItems = 100_000

type finder struct {
	mode    int
	root    string
	gen     int
	input   textinput.Model
	items   []string
	matches []int
	cursor  int
	loading bool
	err     error
}

type finderItemsMsg struct {
	gen   int
	items []string
	err   error
}

func (m *model) openFinder(mode int, query string) tea.Cmd {
	m.finderGen++
	input := textinput.New()
	input.Prompt = "❯ "
	input.Placeholder = "search…"
	input.SetValue(query)
	input.Focus()

	m.finder = &finder{mode: mode, root: m.active().path, gen: m.finderGen, input: input, loading: true}
	return tea.Batch(textinput.Blink, loadFinderItems(mode, m.finder.root, m.showHidden, m.finderGen))
}

func (m *model) updateFinder(msg tea.KeyMsg) tea.Cmd {
	f := m.finder
	switch msg.String() {
	case "esc":
		m.finder = nil
	case "tab":
		return m.openFinder(1-f.mode, f.input.Value())
	case "up", "ctrl+p", "ctrl+k":
		f.cursor = clamp(f.cursor-1, 0, len(f.matches)-1)
	case "down", "ctrl+n", "ctrl+j":
		f.cursor = clamp(f.cursor+1, 0, len(f.matches)-1)
	case "enter":
		m.finder = nil
		if path, ok := f.selectedPath(); ok {
			return m.jumpToPath(path)
		}
	default:
		before := f.input.Value()
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		if f.input.Value() != before {
			f.filter()
		}
		return cmd
	}
	return nil
}

func (f *finder) setItems(items []string, err error) {
	f.items, f.err, f.loading = items, err, false
	f.filter()
}

func (f *finder) filter() {
	f.cursor = 0
	f.matches = f.matches[:0]
	query := f.input.Value()
	if query == "" {
		for i := range f.items {
			f.matches = append(f.matches, i)
		}
		return
	}
	for _, match := range fuzzy.Find(query, f.items) {
		f.matches = append(f.matches, match.Index)
	}
}

func (f *finder) selectedPath() (string, bool) {
	if len(f.matches) == 0 {
		return "", false
	}
	item := f.items[f.matches[f.cursor]]
	if f.mode == modeZoxide {
		return filepath.Clean(item), true
	}
	return filepath.Join(f.root, item), true
}

func (f *finder) view(w, h int) string {
	title := accentStyle.Render(" Files ") + dimStyle.Render(" Zoxide ")
	if f.mode == modeZoxide {
		title = dimStyle.Render(" Files ") + accentStyle.Render(" Zoxide ")
	}
	title += dimStyle.Render("  tab switch · esc close")

	status := ""
	switch {
	case f.loading:
		status = "loading…"
	case f.err != nil:
		status = f.err.Error()
	}
	items := make([]string, len(f.matches))
	for i, idx := range f.matches {
		items[i] = f.items[idx]
	}
	footer := fmt.Sprintf("%d/%d", len(f.matches), len(f.items))
	if f.mode == modeLocal {
		footer += "  " + tildePath(f.root)
	}
	return overlayView(title, &f.input, items, f.cursor, status, f.err != nil, footer, w, h)
}

// overlayView renders a centered box: title, input, scrollable list, footer.
func overlayView(title string, input *textinput.Model, items []string, cursor int, status string, statusIsErr bool, footer string, w, h int) string {
	innerW := max(min(w-4, 100), 10)
	innerH := max(h-2, 5)
	rows := innerH - 4
	input.Width = innerW - 4

	lines := []string{title, " " + input.View(), ""}
	if status != "" {
		style := dimStyle
		if statusIsErr {
			style = errStyle
		}
		lines = append(lines, style.Render(fit(" "+status, innerW)))
	} else {
		offset := max(cursor-rows+1, 0)
		for i := offset; i < len(items) && i < offset+rows; i++ {
			style := fileStyle
			if strings.HasSuffix(items[i], "/") {
				style = dirStyle
			}
			if i == cursor {
				style = style.Background(cursorBg).Foreground(lipgloss.Color("255"))
			}
			lines = append(lines, style.Render(fit(" "+items[i], innerW)))
		}
	}
	for len(lines) < innerH-1 {
		lines = append(lines, "")
	}
	lines = append(lines, dimStyle.Render(fit(" "+footer, innerW)))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Width(innerW).
		Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

func loadFinderItems(mode int, root string, showHidden bool, gen int) tea.Cmd {
	return func() tea.Msg {
		var items []string
		var err error
		if mode == modeZoxide {
			items, err = zoxideList()
		} else {
			items, err = walkDir(root, showHidden)
		}
		return finderItemsMsg{gen: gen, items: items, err: err}
	}
}

var errEnoughItems = errors.New("enough items")

// walkDir lists paths relative to root; directories get a trailing "/".
func walkDir(root string, showHidden bool) ([]string, error) {
	var items []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil // unreadable subfolders are skipped, like fd/fzf do
		}
		if path == root {
			return nil
		}
		name := d.Name()
		skip := name == ".git" || name == "node_modules" || (!showHidden && strings.HasPrefix(name, "."))
		if skip {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			rel += "/"
		}
		items = append(items, rel)
		if len(items) >= maxFinderItems {
			return errEnoughItems
		}
		return nil
	})
	if errors.Is(err, errEnoughItems) {
		err = nil
	}
	return items, err
}

func zoxideList() ([]string, error) {
	if _, err := exec.LookPath("zoxide"); err != nil {
		return nil, errors.New("zoxide not found in PATH")
	}
	out, err := exec.Command("zoxide", "query", "-l").Output()
	if err != nil {
		if len(out) == 0 {
			return nil, errors.New("zoxide database is empty")
		}
		return nil, fmt.Errorf("zoxide: %w", err)
	}
	var items []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			items = append(items, line+"/")
		}
	}
	return items, nil
}
