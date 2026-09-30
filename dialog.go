package main

import (
	"strings"

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
		for i, d := range c.details {
			if i == maxDialogDetails {
				lines = append(lines, dimStyle.Render(fit("  … and more", innerW)))
				break
			}
			lines = append(lines, fileStyle.Render(fit("  "+d, innerW)))
		}
		if len(c.details) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, accentStyle.Render("y")+dimStyle.Render(" confirm · any other key cancels"))
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
