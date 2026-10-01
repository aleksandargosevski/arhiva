package main

import (
	"bufio"
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	maxGrepHits   = 1000
	grepDebounce  = 150 * time.Millisecond
	grepListRatio = 0.4 // share of the box width taken by the result list
)

// grepPanel searches file contents with ripgrep as you type; the right half previews the hit.
type grepPanel struct {
	input      textinput.Model
	root       string
	showHidden bool
	gen        int // bumped on every edit, so stale searches are dropped
	hits       []grepHit
	cursor     int
	loading    bool
	truncated  bool
	err        error
}

type grepHit struct {
	file string // relative to root
	line int
	text string
}

type grepRunMsg struct{ gen int }

type grepResultsMsg struct {
	gen       int
	hits      []grepHit
	truncated bool
	err       error
}

func (m *model) openGrep() tea.Cmd {
	input := textinput.New()
	input.Prompt = "❯ "
	input.Placeholder = "search contents…"
	input.Focus()
	m.grep = &grepPanel{input: input, root: m.active().path, showHidden: m.showHidden}
	if _, err := exec.LookPath("rg"); err != nil {
		m.grep.err = errors.New("ripgrep (rg) not found in PATH, install it with: brew install ripgrep")
	}
	return textinput.Blink
}

func (m *model) updateGrep(msg tea.KeyMsg) tea.Cmd {
	g := m.grep
	switch msg.String() {
	case "esc":
		m.grep = nil
	case "up", "ctrl+p", "ctrl+k":
		g.cursor = clamp(g.cursor-1, 0, len(g.hits)-1)
	case "down", "ctrl+n", "ctrl+j":
		g.cursor = clamp(g.cursor+1, 0, len(g.hits)-1)
	case "enter":
		if len(g.hits) > 0 {
			m.grep = nil
			return m.jumpToPath(filepath.Join(g.root, g.hits[g.cursor].file))
		}
	default:
		before := g.input.Value()
		var cmd tea.Cmd
		g.input, cmd = g.input.Update(msg)
		if g.input.Value() == before {
			return cmd
		}
		g.gen++
		gen := g.gen
		return tea.Batch(cmd, tea.Tick(grepDebounce, func(time.Time) tea.Msg { return grepRunMsg{gen} }))
	}
	return nil
}

// receive handles the debounce tick and the search results; returns the search to run, if any.
func (g *grepPanel) receive(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case grepRunMsg:
		if msg.gen != g.gen {
			return nil
		}
		query := g.input.Value()
		if strings.TrimSpace(query) == "" {
			g.hits, g.cursor, g.loading, g.err = nil, 0, false, nil
			return nil
		}
		g.loading = true
		root, hidden, gen := g.root, g.showHidden, g.gen
		return func() tea.Msg {
			hits, truncated, err := ripgrep(root, query, hidden)
			return grepResultsMsg{gen: gen, hits: hits, truncated: truncated, err: err}
		}
	case grepResultsMsg:
		if msg.gen == g.gen {
			g.hits, g.truncated, g.err, g.cursor, g.loading = msg.hits, msg.truncated, msg.err, 0, false
		}
	}
	return nil
}

// ripgrep runs a literal, smart-case search under root and stops after maxGrepHits.
// ponytail: superseded searches run to completion (their results are dropped); kill them if big trees pile up processes.
func ripgrep(root, query string, hidden bool) ([]grepHit, bool, error) {
	args := []string{"--line-number", "--no-heading", "--null", "--color", "never", "--smart-case",
		"--fixed-strings", "--max-columns", "300", "--max-columns-preview", "--glob", "!.git"}
	if hidden {
		args = append(args, "--hidden")
	}
	cmd := exec.Command("rg", append(args, "--", query)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("rg: %w", err)
	}
	var hits []grepHit
	truncated := false
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if hit, ok := parseGrepLine(scanner.Text()); ok {
			hits = append(hits, hit)
		}
		if len(hits) >= maxGrepHits {
			truncated = true
			_ = cmd.Process.Kill()
			break
		}
	}
	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case truncated:
		err = nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() == 1: // no matches
		err = nil
	case err != nil && len(hits) > 0: // exit 2 with results: some files were unreadable
		err = nil
	case err != nil:
		err = fmt.Errorf("rg: %s", cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return hits, truncated, err
}

// parseGrepLine reads "path\x00line:text" (--null keeps paths with ":" intact).
func parseGrepLine(s string) (grepHit, bool) {
	file, rest, ok := strings.Cut(s, "\x00")
	if !ok {
		return grepHit{}, false
	}
	num, text, ok := strings.Cut(rest, ":")
	line, err := strconv.Atoi(num)
	if !ok || err != nil {
		return grepHit{}, false
	}
	return grepHit{file: file, line: line, text: text}, true
}

func (g *grepPanel) view(theme string, w, h int) string {
	innerW := max(w-4, 20)
	innerH := max(h-2, 5)
	listW := max(int(float64(innerW)*grepListRatio), 20)
	previewW := max(innerW-listW-1, 1)
	rows := innerH - 4
	g.input.Width = listW - 4

	left := []string{
		accentStyle.Render(" Grep ") + dimStyle.Render(" enter go · esc close"),
		" " + g.input.View(),
		"",
	}
	if g.err != nil {
		left = append(left, errStyle.Render(fit(" "+g.err.Error(), listW)))
	}
	offset := max(g.cursor-rows+1, 0)
	for i := offset; i < len(g.hits) && i < offset+rows && g.err == nil; i++ {
		hit := g.hits[i]
		loc := fmt.Sprintf("%s:%d ", hit.file, hit.line)
		text := sanitize(strings.TrimSpace(hit.text))
		if i == g.cursor {
			sel := lipgloss.NewStyle().Background(cursorBg).Foreground(lipgloss.Color("255"))
			left = append(left, sel.Render(fit(" "+loc+text, listW)))
			continue
		}
		locW := min(len([]rune(loc))+1, listW)
		left = append(left, dimStyle.Render(fit(" "+loc, locW))+fileStyle.Render(fit(text, listW-locW)))
	}
	for len(left) < innerH-1 {
		left = append(left, "")
	}
	footer := fmt.Sprintf("%d", len(g.hits))
	switch {
	case g.loading:
		footer = "searching…"
	case g.truncated:
		footer += "+ (first " + strconv.Itoa(maxGrepHits) + ")"
	}
	left = append(left, dimStyle.Render(fit(" "+footer+"  "+tildePath(g.root)+"  · rg", listW)))

	right := g.previewLines(theme, previewW, innerH)
	lines := make([]string, innerH)
	sep := dimStyle.Render("│")
	for i := range lines {
		lines[i] = padRight(left[i], listW) + sep + padRight(right[i], previewW)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Render(strings.Join(lines, "\n"))
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// previewLines shows the file around the selected hit with line numbers, the hit line marked.
// ponytail: only the visible window is highlighted, so a hit inside a long block comment may be colored as code.
func (g *grepPanel) previewLines(theme string, w, h int) []string {
	out := make([]string, h)
	if len(g.hits) == 0 {
		return out
	}
	hit := g.hits[g.cursor]
	path := filepath.Join(g.root, hit.file)
	out[0] = accentStyle.Render(fit(" "+hit.file, w))
	data, err := os.ReadFile(path)
	if err != nil {
		out[1] = errStyle.Render(fit(" "+err.Error(), w))
		return out
	}
	all := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	rows := h - 1
	start := clamp(hit.line-1-rows/2, 0, max(len(all)-rows, 0))
	end := min(start+rows, len(all))
	colored := highlight(path, sanitize(strings.Join(all[start:end], "\n")), theme)
	numW := len(strconv.Itoa(end))
	for i, l := range colored {
		n := start + i + 1
		gutter := dimStyle.Render(fmt.Sprintf(" %*d ", numW, n))
		if n == hit.line {
			gutter = accentStyle.Render(fmt.Sprintf("▶%*d ", numW, n))
		}
		out[i+1] = gutter + ansi.Truncate(l, max(w-numW-2, 0), "")
	}
	return out
}

// padRight pads a styled string to exactly w cells, truncating if longer.
func padRight(s string, w int) string {
	s = ansi.Truncate(s, w, "")
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}
