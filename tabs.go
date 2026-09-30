package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// tab holds the per-tab state of inactive tabs; the active tab lives in model.cols/back/fwd.
// Selection, clipboard and undo are shared, so you can copy in one tab and paste in another.
type tab struct {
	cols      []*column
	back, fwd []location
}

func (m *model) saveTab() { m.tabs[m.tabIdx] = tab{m.cols, m.back, m.fwd} }

func (m *model) switchTab(i int) {
	if i < 0 || i >= len(m.tabs) {
		m.setErr(fmt.Errorf("no tab %d", i+1))
		return
	}
	if i == m.tabIdx {
		return
	}
	m.saveTab()
	m.loadTab(i)
}

func (m *model) loadTab(i int) {
	t := m.tabs[i]
	m.tabIdx, m.cols, m.back, m.fwd = i, t.cols, t.back, t.fwd
	m.visual, m.sidebarFocus = nil, false
	m.reloadColumns() // it may have changed while in the background
}

func (m *model) newTab() {
	col, err := loadColumn(m.active().path, m.listOpts())
	if err != nil {
		m.setErr(err)
		return
	}
	if e, ok := m.active().selected(); ok {
		col.selectName(e.name)
	}
	m.saveTab()
	m.tabs = append(m.tabs, tab{cols: []*column{col}})
	m.loadTab(len(m.tabs) - 1)
}

func (m *model) closeTab() {
	if len(m.tabs) == 1 {
		m.setErr(errors.New("last tab, press q to quit"))
		return
	}
	m.tabs = append(m.tabs[:m.tabIdx], m.tabs[m.tabIdx+1:]...)
	m.loadTab(min(m.tabIdx, len(m.tabs)-1))
}

func (m *model) tabBar() string {
	if len(m.tabs) < 2 {
		return ""
	}
	var parts []string
	for i, t := range m.tabs {
		path := m.active().path
		if i != m.tabIdx {
			path = t.cols[len(t.cols)-1].path
		}
		label := fmt.Sprintf(" %d %s ", i+1, runeTruncate(filepath.Base(path), 12))
		if i == m.tabIdx {
			parts = append(parts, tabActiveStyle.Render(label))
		} else {
			parts = append(parts, dimStyle.Render(label))
		}
	}
	return strings.Join(parts, "")
}

func runeTruncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
