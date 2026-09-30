package main

import (
	"errors"
	"path/filepath"
)

const maxHistory = 100

// location is a full column layout, so jumping back restores every column, not just the folder.
type location struct {
	paths  []string
	cursor string // entry under the cursor in the last column
}

func (m *model) currentLocation() location {
	loc := location{}
	for _, c := range m.cols {
		loc.paths = append(loc.paths, c.path)
	}
	if e, ok := m.active().selected(); ok {
		loc.cursor = e.name
	}
	return loc
}

// recordJump saves the current location before a jump; a new jump drops the forward history.
func (m *model) recordJump() {
	m.back = append(m.back, m.currentLocation())
	if len(m.back) > maxHistory {
		m.back = m.back[1:]
	}
	m.fwd = nil
}

// historyStep moves from one stack to the other: back/forward in the browser sense.
// Locations that no longer exist are skipped.
func (m *model) historyStep(from, to *[]location) {
	for len(*from) > 0 {
		loc := (*from)[len(*from)-1]
		*from = (*from)[:len(*from)-1]
		cur := m.currentLocation()
		if m.restore(loc) {
			*to = append(*to, cur)
			return
		}
	}
	m.setErr(errors.New("no more history"))
}

func (m *model) restore(loc location) bool {
	var cols []*column
	for _, p := range loc.paths {
		c, err := loadColumn(p, m.listOpts())
		if err != nil {
			break
		}
		if len(cols) > 0 {
			cols[len(cols)-1].selectName(filepath.Base(p))
		}
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return false
	}
	cols[len(cols)-1].selectName(loc.cursor)
	m.cols = cols
	m.sidebarFocus = false
	return true
}
