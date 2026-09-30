package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var gitColors = map[byte]lipgloss.Color{'M': "221", '+': "114", '?': "245", '-': "203", 'R': "81", '!': "203"}

// gitState caches `git status` per repository. Invalidating bumps gen, so columns re-request
// while the old marks stay visible until fresh ones arrive.
type gitState struct {
	enabled bool
	gen     int
	roots   map[string]string          // folder → repo root ("" = not in a repo)
	marks   map[string]map[string]byte // repo root → absolute path → mark
	askedAt map[string]int             // folder or root → gen of its last request
}

type gitMsg struct {
	dir, root string
	marks     map[string]byte
}

func newGitState(enabled bool) gitState {
	return gitState{enabled: enabled, roots: map[string]string{}, marks: map[string]map[string]byte{}, askedAt: map[string]int{}}
}

func (g *gitState) invalidate() { g.gen++ }

func (g *gitState) receive(msg gitMsg) {
	g.roots[msg.dir] = msg.root
	if msg.root != "" {
		g.marks[msg.root] = msg.marks
	}
}

func (g *gitState) mark(dir, path string) byte {
	return g.marks[g.roots[dir]][path]
}

// syncGit requests status for visible folders that are outdated, once per repo.
func (m *model) syncGit() tea.Cmd {
	g := &m.git
	if !g.enabled {
		return nil
	}
	var cmds []tea.Cmd
	for _, c := range m.cols {
		if g.askedAt[c.path] == g.gen+1 {
			continue
		}
		g.askedAt[c.path] = g.gen + 1 // +1 so the zero value means "never asked"
		if root := g.roots[c.path]; root != "" {
			if g.askedAt[root] == g.gen+1 {
				continue
			}
			g.askedAt[root] = g.gen + 1
		}
		cmds = append(cmds, loadGit(c.path))
	}
	return tea.Batch(cmds...)
}

func loadGit(dir string) tea.Cmd {
	return func() tea.Msg {
		// The prefix (dir relative to the repo root) gives the root in our own spelling of the
		// path, unlike --show-toplevel which resolves symlinks like /tmp → /private/tmp.
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-prefix").Output()
		if err != nil {
			return gitMsg{dir: dir}
		}
		prefix := strings.TrimSpace(string(out))
		root := filepath.Clean(strings.TrimSuffix(dir+"/", prefix))
		status, err := exec.Command("git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=normal").Output()
		if err != nil {
			return gitMsg{dir: dir}
		}
		return gitMsg{dir: dir, root: root, marks: parseGitStatus(root, status)}
	}
}

// parseGitStatus turns porcelain v1 -z output into marks for files and every folder above them.
func parseGitStatus(root string, out []byte) map[string]byte {
	marks := map[string]byte{}
	fields := bytes.Split(out, []byte{0})
	for i := 0; i < len(fields); i++ {
		f := string(fields[i])
		if len(f) < 4 {
			continue
		}
		xy, rel := f[:2], f[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the original path follows
		}
		mark := gitMark(xy)
		path := filepath.Join(root, strings.TrimSuffix(rel, "/"))
		marks[path] = mark
		for dir := filepath.Dir(path); dir != root && strings.HasPrefix(dir, root); dir = filepath.Dir(dir) {
			if old, ok := marks[dir]; !ok || (old == '?' && mark != '?') {
				marks[dir] = mark
			}
		}
	}
	return marks
}

func gitMark(xy string) byte {
	switch {
	case xy == "??":
		return '?'
	case strings.ContainsRune(xy, 'U') || xy == "AA" || xy == "DD":
		return '!'
	case strings.ContainsRune(xy, 'A'):
		return '+'
	case strings.ContainsRune(xy, 'D'):
		return '-'
	case strings.ContainsRune(xy, 'R'):
		return 'R'
	}
	return 'M'
}
