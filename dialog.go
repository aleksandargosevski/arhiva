package main

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const maxDialogDetails = 8

var dialogBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

// dialogView renders the active prompt or confirmation as a box, or "" when there is none.
func (m *model) dialogView() string {
	innerW := clamp(m.width-8, 20, 70)
	var lines []string
	border := lipgloss.Color("214")

	switch {
	case m.confirm != nil:
		c := m.confirm
		border = lipgloss.Color("203")
		lines = append(lines, errStyle.Bold(true).Render(fit(c.question, innerW)), "")
		lines = append(lines, detailLines(c.details, innerW)...)
		lines = append(lines, accentStyle.Render("y")+dimStyle.Render(" confirm · any other key cancels"))
	case m.choice != nil:
		c := m.choice
		lines = append(lines, accentStyle.Render(fit(c.title, innerW)), "")
		lines = append(lines, detailLines(c.details, innerW)...)
		for i, opt := range c.options {
			if i == c.cursor {
				lines = append(lines, accentStyle.Render(fit("▸ "+opt, innerW)))
			} else {
				lines = append(lines, fileStyle.Render(fit("  "+opt, innerW)))
			}
		}
		lines = append(lines, "", accentStyle.Render("j/k")+dimStyle.Render(" move · ")+accentStyle.Render("enter")+dimStyle.Render(" select · ")+accentStyle.Render("esc")+dimStyle.Render(" cancel"))
	case m.prompt != nil && !m.prompt.inline:
		p := m.prompt
		p.input.Width = innerW - 1
		lines = append(lines, accentStyle.Render(fit(p.title, innerW)), "", p.input.View(), "",
			accentStyle.Render("enter")+dimStyle.Render(" confirm · ")+accentStyle.Render("esc")+dimStyle.Render(" cancel"))
	default:
		return ""
	}
	return dialogBorder.BorderForeground(border).Width(innerW + 2).Render(strings.Join(lines, "\n"))
}

// detailLines lists names under a dialog title, capped at maxDialogDetails, followed by a blank line.
func detailLines(details []string, innerW int) []string {
	var lines []string
	for i, d := range details {
		if i == maxDialogDetails {
			lines = append(lines, dimStyle.Render(fit("  … and more", innerW)))
			break
		}
		lines = append(lines, fileStyle.Render(fit("  "+d, innerW)))
	}
	if len(details) > 0 {
		lines = append(lines, "")
	}
	return lines
}

// choice is a pick-one list shown as a dialog; j/k or arrows move, enter picks, esc cancels.
type choice struct {
	key     string // remembers the last pick in model.lastPick; "" always starts on the first option
	title   string
	details []string // optional, e.g. the names an operation affects
	options []string
	cursor  int
	pick    func(string) tea.Cmd
}

func (m *model) openChoice(key, title string, options []string, pick func(string) tea.Cmd) {
	cursor := 0
	if key != "" {
		cursor = max(slices.Index(options, m.lastPick[key]), 0)
	}
	m.choice = &choice{key: key, title: title, options: options, cursor: cursor, pick: pick}
}

func (m *model) updateChoice(msg tea.KeyMsg) tea.Cmd {
	c := m.choice
	switch msg.String() {
	case "esc", "q":
		m.choice = nil
	case "j", "down", "ctrl+n", "tab":
		c.cursor = (c.cursor + 1) % len(c.options)
	case "k", "up", "ctrl+p", "shift+tab":
		c.cursor = (c.cursor + len(c.options) - 1) % len(c.options)
	case "g", "home":
		c.cursor = 0
	case "G", "end":
		c.cursor = len(c.options) - 1
	case "enter", "l", "right":
		m.choice = nil
		picked := c.options[c.cursor]
		if c.key != "" {
			m.lastPick[c.key] = picked
		}
		return c.pick(picked)
	}
	return nil
}

// overlayCenter draws box over the middle of bg, keeping bg visible around it.
func overlayCenter(bg, box string, width int) string {
	bgLines := strings.Split(bg, "\n")
	boxLines := strings.Split(box, "\n")
	boxW := lipgloss.Width(box)
	top := max((len(bgLines)-len(boxLines))/2, 0)
	left := max((width-boxW)/2, 0)
	for i, bl := range boxLines {
		y := top + i
		if y >= len(bgLines) {
			break
		}
		line := bgLines[y]
		// Wc variants measure like go-runewidth (and the terminal): Nerd Font icons are one cell.
		bgLines[y] = ansi.TruncateWc(line, left, "") + "\x1b[0m" + bl + "\x1b[0m" + ansi.TruncateLeftWc(line, left+boxW, "") // CutWc is buggy in x/ansi v0.11
	}
	return strings.Join(bgLines, "\n")
}
