package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	tea "github.com/charmbracelet/bubbletea"
)

const maxUndo = 50

type move struct{ from, to string }

// undoStep describes how to revert one operation. Trash and permanent delete are not undoable
// (use Finder's "Put Back" for Trash).
type undoStep struct {
	desc    string
	moves   []move   // reverted by moving each `to` back to `from`
	created []string // reverted by moving to Trash
}

func (m *model) pushUndo(u *undoStep) {
	if u == nil || len(u.moves)+len(u.created) == 0 {
		return
	}
	m.undoStack = append(m.undoStack, u)
	if len(m.undoStack) > maxUndo {
		m.undoStack = m.undoStack[1:]
	}
}

func (m *model) undo() tea.Cmd {
	if len(m.undoStack) == 0 {
		m.setErr(errors.New("nothing to undo"))
		return nil
	}
	u := m.undoStack[len(m.undoStack)-1]
	m.undoStack = m.undoStack[:len(m.undoStack)-1]
	return func() tea.Msg {
		// Two phases through temporary names, so undoing a swap (a→b, b→a) works.
		var errs []error
		tmps := map[int]string{}
		for i, mv := range u.moves {
			tmp := filepath.Join(filepath.Dir(mv.to), fmt.Sprintf(".arhiva-undo-%d-%s", i, filepath.Base(mv.to)))
			if err := os.Rename(mv.to, tmp); err != nil {
				errs = append(errs, err)
				continue
			}
			tmps[i] = tmp
		}
		for i, mv := range u.moves {
			tmp, ok := tmps[i]
			if !ok {
				continue
			}
			err := fmt.Errorf("%s exists", tildePath(mv.from))
			if !exists(mv.from) {
				err = moveFile(tmp, mv.from)
			}
			if err != nil {
				errs = append(errs, err)
				os.Rename(tmp, mv.to) // leave it where it was
			}
		}
		created := slices.DeleteFunc(slices.Clone(u.created), func(p string) bool { return !exists(p) })
		if len(created) > 0 {
			if err := moveToTrash(created); err != nil {
				errs = append(errs, err)
			}
		}
		return opDoneMsg{status: "Undid " + u.desc, err: errors.Join(errs...)}
	}
}
