package main

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const (
	sidebarWidth   = 24
	minColumnWidth = 20
)

var (
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	dirStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true)
	fileStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	linkStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("80"))
	headerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	errStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	accentStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	tabActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Background(lipgloss.Color("25")).Bold(true)
	selectColor    = lipgloss.Color("214")
	cursorBg       = lipgloss.Color("25")
	inactiveBg     = lipgloss.Color("237")
	visualBg       = lipgloss.Color("60")
)

type model struct {
	cfg       Config
	bindings  map[string]string
	bookmarks []Bookmark
	cols      []*column
	back, fwd []location // jump history
	tabs      []tab
	tabIdx    int

	showSidebar   bool
	showHidden    bool
	sortBy        string
	sortReverse   bool
	sidebarFocus  bool
	sidebarCursor int

	selected  map[string]bool // absolute paths, kept across folders
	visual    *visualRange
	clip      clipboard
	jobs      []*job
	undoStack []*undoStep
	prompt    *prompt
	confirm   *confirm

	width, height int
	pending       string
	status        string
	statusIsErr   bool

	finder    *finder
	gotoPanel *gotoPanel
	finderGen int
	help      *help

	preview  preview
	git      gitState
	dirSizes map[string]int64 // calculated on demand with S

	cdOnQuit   bool
	hasZoxide  bool
	chooseFile string    // picker mode: opening a file writes the targets here and quits
	cfgModTime time.Time // to reload the config when it's saved
}

type errMsg struct{ err error }

// visualRange is an in-progress vim-like range selection in one column.
type visualRange struct {
	col    *column
	anchor int
}

func newModel(cfg Config, startDir string) (*model, error) {
	m := &model{
		cfg:         cfg,
		showSidebar: cfg.ShowSidebar,
		showHidden:  cfg.ShowHidden,
		sortBy:      cmp.Or(cfg.Sort, "name"),
		preview:     newPreview(cfg),
		git:         newGitState(cfg.Git),
		selected:    map[string]bool{},
		dirSizes:    map[string]int64{},
		cfgModTime:  configModTime(),
	}
	_, err := exec.LookPath("zoxide")
	m.hasZoxide = err == nil

	col, err := loadColumn(startDir, m.listOpts())
	if err != nil {
		return nil, err
	}
	m.cols = []*column{col}
	m.tabs = []tab{{}}

	m.bindings, m.bookmarks, err = buildBindings(cfg)
	if err != nil {
		m.setErr(err)
	}
	return m, nil
}

type tickMsg struct{}

// ponytail: polling twice a second instead of fsnotify; a few stats per tick, changes show up within 0.5s.
const tickInterval = 500 * time.Millisecond

func tick() tea.Cmd { return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m *model) Init() tea.Cmd { return tick() }

// refreshIfChanged reloads columns whose folder changed on disk, and the preview if its file changed.
func (m *model) refreshIfChanged() {
	for _, c := range m.cols {
		if info, err := os.Stat(c.path); err != nil || !info.ModTime().Equal(c.modTime) {
			m.reloadColumns()
			m.preview.refresh()
			m.git.invalidate()
			return
		}
	}
	// ponytail: edits to files outside the preview don't change folder mtimes, so their git marks
	// update on the next folder change; poll `git status` periodically if that matters.
	if m.preview.refreshIfChanged() {
		m.git.invalidate()
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	return m, tea.Batch(cmd, m.syncPreview(), m.syncGit())
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tickMsg:
		m.refreshIfChanged()
		m.reloadConfigIfChanged()
		return tick()
	case gitMsg:
		m.git.receive(msg)
	case dirSizeMsg:
		maps.Copy(m.dirSizes, msg.sizes)
		m.status, m.statusIsErr = msg.status, false
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.preview.updateCellSize()
	case previewMsg:
		m.preview.receive(msg)
	case editorDoneMsg:
		m.preview.refresh()
		m.git.invalidate()
		if msg.err != nil {
			m.setErr(fmt.Errorf("editor: %w", msg.err))
		}
	case opDoneMsg:
		m.jobs = slices.DeleteFunc(m.jobs, func(j *job) bool { return j == msg.job })
		m.afterOp(msg.selectName)
		m.git.invalidate()
		m.pushUndo(msg.undo) // partial successes are recorded too
		if msg.err != nil {
			m.setErr(msg.err)
		} else {
			m.status, m.statusIsErr = msg.status, false
		}
	case errMsg:
		m.setErr(msg.err)
	case finderItemsMsg:
		if m.finder != nil && m.finder.gen == msg.gen {
			m.finder.setItems(msg.items, msg.err)
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return tea.Quit
		}
		switch {
		case m.confirm != nil:
			return m.updateConfirm(msg)
		case m.prompt != nil:
			return m.updatePrompt(msg)
		case m.finder != nil:
			return m.updateFinder(msg)
		case m.gotoPanel != nil:
			return m.updateGoto(msg)
		case m.help != nil:
			return m.updateHelp(msg)
		default:
			m.status = ""
			// Fast input can arrive as one multi-rune key ("nnotes"). Each rune goes through update
			// again, so once one opens a prompt the rest is typed into it instead of run as commands.
			if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
				var cmds []tea.Cmd
				for _, r := range msg.Runes {
					cmds = append(cmds, m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}))
				}
				return tea.Batch(cmds...)
			}
			return m.handleKey(msg.String())
		}
	}
	return nil
}

// handleKey resolves multi-key sequences like "gg" or "gd". Named keys ("right", "ctrl+u") are atomic:
// they never continue or start a sequence.
func (m *model) handleKey(key string) tea.Cmd {
	if key == " " {
		key = "space"
	}
	if key == "esc" {
		switch {
		case m.pending != "":
			m.pending = ""
		case m.visual != nil:
			m.visual = nil
		case m.active().filter != "":
			m.active().setFilter("")
		default:
			clear(m.selected)
		}
		return nil
	}
	if isNamedKey(key) {
		m.pending = ""
		return m.run(m.bindings[key])
	}
	if _, bound := m.bindings[key]; !bound && m.pending == "" && len(key) == 1 && key[0] >= '1' && key[0] <= '9' {
		m.switchTab(int(key[0] - '1'))
		return nil
	}
	seq := m.pending + key
	m.pending = ""
	if action, ok := m.bindings[seq]; ok {
		return m.run(action)
	}
	for k := range m.bindings {
		if !isNamedKey(k) && strings.HasPrefix(k, seq) {
			m.pending = seq
			return nil
		}
	}
	if seq != key {
		return m.handleKey(key)
	}
	return nil
}

func (m *model) run(action string) tea.Cmd {
	cmd := m.runAction(action)
	// Visual mode only lives while its column is active and focused.
	if m.visual != nil && (m.visual.col != m.active() || m.sidebarFocus) {
		m.visual = nil
	}
	return cmd
}

func (m *model) runAction(action string) tea.Cmd {
	switch action {
	case "up":
		m.move(-1)
	case "down":
		m.move(1)
	case "page_up":
		m.move(-m.bodyHeight() / 2)
	case "page_down":
		m.move(m.bodyHeight() / 2)
	case "top":
		m.move(-1 << 30)
	case "bottom":
		m.move(1 << 30)
	case "left":
		if !m.sidebarFocus {
			return m.goLeft()
		}
	case "right", "open":
		if m.sidebarFocus {
			m.recordJump()
			return m.resetTo(m.bookmarks[m.sidebarCursor].Path, "")
		}
		if e, ok := m.active().selected(); ok && !e.isDir && action == "right" {
			return nil // only "open" opens files, so an extra l doesn't launch an app
		}
		return m.goRight()
	case "new_tab":
		m.newTab()
	case "close_tab":
		m.closeTab()
	case "next_tab":
		m.switchTab((m.tabIdx + 1) % len(m.tabs))
	case "prev_tab":
		m.switchTab((m.tabIdx + len(m.tabs) - 1) % len(m.tabs))
	case "jump_back":
		m.historyStep(&m.back, &m.fwd)
	case "jump_forward":
		m.historyStep(&m.fwd, &m.back)
	case "set_root":
		if !m.sidebarFocus && len(m.cols) > 1 {
			col := m.active()
			m.cols = []*column{col}
		}
	case "toggle_sidebar":
		m.showSidebar = !m.showSidebar
		m.sidebarFocus = m.sidebarFocus && m.showSidebar
	case "focus":
		if m.showSidebar && len(m.bookmarks) > 0 {
			m.sidebarFocus = !m.sidebarFocus
		}
	case "toggle_hidden":
		m.toggleHidden()
	case "filter":
		if !m.sidebarFocus {
			return m.startFilter()
		}
	case "sort_name", "sort_modified", "sort_size", "sort_ext":
		m.sortBy = strings.TrimPrefix(action, "sort_")
		m.reloadColumns()
		m.status, m.statusIsErr = "Sorted by "+m.sortBy, false
	case "sort_reverse":
		m.sortReverse = !m.sortReverse
		m.reloadColumns()
	case "find":
		return m.openFinder(modeLocal, "")
	case "zoxide":
		return m.openFinder(modeZoxide, "")
	case "goto":
		return m.openGoto()
	case "select":
		if !m.sidebarFocus {
			m.toggleSelect()
		}
	case "visual":
		switch {
		case m.visual != nil:
			m.visual = nil
		case !m.sidebarFocus && len(m.active().entries) > 0:
			m.visual = &visualRange{col: m.active(), anchor: m.active().cursor}
		}
	case "invert_selection":
		if !m.sidebarFocus {
			m.invertSelection()
		}
	case "new", "copy", "cut", "paste", "zip", "rename", "rename_append", "rename_replace":
		if !m.sidebarFocus {
			return m.fileAction(action)
		}
	case "undo":
		return m.undo()
	case "tags":
		if !m.sidebarFocus {
			return m.startTags()
		}
	case "duplicate":
		if !m.sidebarFocus {
			return m.duplicate()
		}
	case "copy_contents":
		if !m.sidebarFocus {
			return m.copyContents()
		}
	case "airdrop":
		if !m.sidebarFocus {
			return m.airDrop()
		}
	case "symlink":
		if !m.sidebarFocus {
			m.symlink()
		}
	case "diff":
		if !m.sidebarFocus {
			return m.diff()
		}
	case "command":
		return m.openPrompt(`Command  ($f cursor, $d folder, "$@" targets)`, "", 0, func(run string) tea.Cmd {
			if run == "" {
				return nil
			}
			return m.runCommand(Command{Run: run})
		})
	case "preview_down":
		m.preview.scrollBy(m.bodyHeight()/2, m.bodyHeight())
	case "preview_up":
		m.preview.scrollBy(-m.bodyHeight()/2, m.bodyHeight())
	case "quick_look":
		if paths := m.targets(); len(paths) > 0 && !m.sidebarFocus {
			return quickLookPanel(paths)
		}
	case "dir_size":
		if paths := m.targets(); len(paths) > 0 && !m.sidebarFocus {
			m.status, m.statusIsErr = "Calculating size…", false
			return calcSizes(paths)
		}
	case "yank":
		m.yank()
	case "bookmark":
		if !m.sidebarFocus {
			m.addBookmark()
		}
	case "trash":
		if m.sidebarFocus {
			m.removeBookmark(m.sidebarCursor)
		} else {
			m.startDelete(false)
		}
	case "delete":
		if !m.sidebarFocus {
			m.startDelete(true)
		}
	case "help":
		m.help = newHelp(m.bindings, m.bookmarks, m.cfg.Commands)
	case "quit", "quit_no_cd":
		quit := func() tea.Cmd {
			m.cdOnQuit = action == "quit"
			return tea.Quit
		}
		if len(m.jobs) > 0 {
			m.confirm = &confirm{question: "An operation is still running and would continue unwatched. Quit?", yes: quit}
			return nil
		}
		return quit()
	default:
		if idx, ok := strings.CutPrefix(action, "bookmark:"); ok {
			i, _ := strconv.Atoi(idx)
			m.recordJump()
			return m.resetTo(m.bookmarks[i].Path, "")
		}
		if idx, ok := strings.CutPrefix(action, "cmd:"); ok {
			i, _ := strconv.Atoi(idx)
			return m.runCommand(m.cfg.Commands[i])
		}
	}
	return nil
}

// runCommand runs a user command with `sh -c` in the current folder. The command gets
// $f (entry under cursor), $d (current folder), $fx (targets, newline separated) and the targets as "$@".
func (m *model) runCommand(c Command) tea.Cmd {
	col := m.active()
	targets := m.targets()
	current := ""
	if e, ok := col.selected(); ok {
		current = filepath.Join(col.path, e.name)
	}
	cmd := exec.Command("sh", append([]string{"-c", c.Run, "arhiva"}, targets...)...)
	cmd.Dir = col.path
	cmd.Env = append(os.Environ(), "f="+current, "d="+col.path, "fx="+strings.Join(targets, "\n"))
	m.clearSelection()
	label := cmp.Or(c.Desc, c.Run)

	if c.Interactive {
		return tea.ExecProcess(cmd, func(err error) tea.Msg {
			return opDoneMsg{status: "Ran " + label, err: err}
		})
	}
	m.status, m.statusIsErr = "Running "+label+"…", false
	return func() tea.Msg {
		out, err := cmd.CombinedOutput()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		last := lines[len(lines)-1]
		if err != nil {
			return opDoneMsg{err: fmt.Errorf("%s: %s", label, cmp.Or(last, err.Error()))}
		}
		return opDoneMsg{status: cmp.Or(last, "Ran "+label)}
	}
}

func (m *model) active() *column { return m.cols[len(m.cols)-1] }

func (m *model) fileAction(action string) tea.Cmd {
	switch action {
	case "new":
		return m.startNew()
	case "copy":
		m.copyToClipboard(false)
	case "cut":
		m.copyToClipboard(true)
	case "paste":
		return m.paste()
	case "zip":
		if paths := m.targets(); len(paths) > 0 && all(paths, isArchive) {
			return m.extract(paths)
		}
		return m.startZip()
	default:
		return m.startRename(action)
	}
	return nil
}

// toggleSelect toggles the entry under the cursor and moves down. In visual mode it toggles the
// range as a whole: if every entry in it is already selected they get unselected, otherwise all get selected.
func (m *model) toggleSelect() {
	col := m.active()
	if m.visual != nil {
		lo, hi := m.visual.bounds()
		paths := make([]string, 0, hi-lo+1)
		allSelected := true
		for _, e := range col.entries[lo : hi+1] {
			p := filepath.Join(col.path, e.name)
			paths = append(paths, p)
			allSelected = allSelected && m.selected[p]
		}
		for _, p := range paths {
			if allSelected {
				delete(m.selected, p)
			} else {
				m.selected[p] = true
			}
		}
		m.visual = nil
		return
	}
	e, ok := col.selected()
	if !ok {
		return
	}
	path := filepath.Join(col.path, e.name)
	if m.selected[path] {
		delete(m.selected, path)
	} else {
		m.selected[path] = true
	}
	m.move(1)
}

// invertSelection flips the selection of every entry in the active column; other folders are untouched.
func (m *model) invertSelection() {
	col := m.active()
	for _, e := range col.entries {
		p := filepath.Join(col.path, e.name)
		if m.selected[p] {
			delete(m.selected, p)
		} else {
			m.selected[p] = true
		}
	}
	m.visual = nil
}

func (v *visualRange) bounds() (lo, hi int) {
	return min(v.anchor, v.col.cursor), max(v.anchor, v.col.cursor)
}

func (m *model) move(delta int) {
	if m.sidebarFocus {
		m.sidebarCursor = clamp(m.sidebarCursor+delta, 0, len(m.bookmarks)-1)
		return
	}
	c := m.active()
	c.cursor = clamp(c.cursor+delta, 0, len(c.entries)-1)
}

func (m *model) goRight() tea.Cmd {
	col := m.active()
	e, ok := col.selected()
	if !ok {
		return nil
	}
	path := filepath.Join(col.path, e.name)
	if !e.isDir {
		if m.chooseFile != "" {
			return m.choose()
		}
		return openFile(path)
	}
	next, err := loadColumn(path, m.listOpts())
	if err != nil {
		m.setErr(err)
		return nil
	}
	m.cols = append(m.cols, next)
	return m.zoxideAdd(path)
}

func (m *model) goLeft() tea.Cmd {
	if len(m.cols) > 1 {
		m.cols = m.cols[:len(m.cols)-1]
		return nil
	}
	cur := m.cols[0].path
	parent := filepath.Dir(cur)
	if parent == cur {
		return nil
	}
	return m.resetTo(parent, filepath.Base(cur))
}

// resetTo makes dir the only column, optionally placing the cursor on selectName.
func (m *model) resetTo(dir, selectName string) tea.Cmd {
	col, err := loadColumn(dir, m.listOpts())
	if err != nil {
		m.setErr(err)
		return nil
	}
	col.selectName(selectName)
	m.cols = []*column{col}
	m.sidebarFocus = false
	return m.zoxideAdd(dir)
}

func (m *model) jumpToPath(path string) tea.Cmd {
	info, err := os.Stat(path)
	if err != nil {
		m.setErr(err)
		return nil
	}
	m.recordJump()
	if info.IsDir() {
		return m.resetTo(path, "")
	}
	name := filepath.Base(path)
	if strings.HasPrefix(name, ".") {
		m.showHidden = true
	}
	return m.resetTo(filepath.Dir(path), name)
}

func (m *model) listOpts() listOpts {
	return listOpts{showHidden: m.showHidden, sortBy: m.sortBy, reverse: m.sortReverse}
}

// startFilter narrows the active column live while typing; enter keeps the filter, esc restores the old one.
func (m *model) startFilter() tea.Cmd {
	col := m.active()
	old := col.filter
	cmd := m.openPrompt("Filter", old, len([]rune(old)), func(string) tea.Cmd { return nil })
	m.prompt.change = col.setFilter
	m.prompt.inline = true
	m.prompt.cancel = func() { col.setFilter(old) }
	return cmd
}

func (m *model) toggleHidden() {
	m.showHidden = !m.showHidden
	m.reloadColumns()
}

// reloadColumns re-reads all columns in place, keeping cursors and filters. If a column's child
// folder disappeared (deleted, renamed, or now hidden), deeper columns are dropped.
func (m *model) reloadColumns() {
	for i, col := range m.cols {
		fresh, err := loadColumn(col.path, m.listOpts())
		if err != nil {
			m.setErr(err)
			m.cols = m.cols[:i]
			break
		}
		prev, hadPrev := col.selected()
		prevCursor := col.cursor
		col.all, col.modTime = fresh.all, fresh.modTime
		col.setFilter(col.filter)
		if i < len(m.cols)-1 {
			if !col.selectName(filepath.Base(m.cols[i+1].path)) {
				m.cols = m.cols[:i+1]
				break
			}
			continue
		}
		if !hadPrev || !col.selectName(prev.name) {
			col.cursor = clamp(prevCursor, 0, len(col.entries)-1)
		}
	}
	if len(m.cols) == 0 { // first column became unreadable; stay on home
		col, _ := loadColumn(homeDir(), m.listOpts())
		m.cols = []*column{col}
	}
}

// yank copies the target paths (one per line) to the system clipboard.
func (m *model) yank() {
	paths := []string{m.active().path}
	if m.sidebarFocus {
		paths = []string{m.bookmarks[m.sidebarCursor].Path}
	} else if t := m.targets(); len(t) > 0 {
		paths = t
	}
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n"))
	if err := cmd.Run(); err != nil {
		m.setErr(fmt.Errorf("copy failed: %w", err))
		return
	}
	label := tildePath(paths[0])
	if len(paths) > 1 {
		label = fmt.Sprintf("%d paths", len(paths))
	}
	m.status, m.statusIsErr = "Copied "+label, false
}

// addBookmark bookmarks the folder under the cursor, or the current folder when the cursor is on a file.
func (m *model) addBookmark() {
	col := m.active()
	path := col.path
	if e, ok := col.selected(); ok && e.isDir {
		path = filepath.Join(col.path, e.name)
	}
	for _, b := range m.bookmarks {
		if b.Path == path {
			m.setErr(fmt.Errorf("%s is already bookmarked (%s)", tildePath(path), b.Key))
			return
		}
	}
	if m.setBookmarks(append(slices.Clone(m.cfg.Bookmarks), path)) {
		b := m.bookmarks[len(m.bookmarks)-1]
		m.status, m.statusIsErr = fmt.Sprintf("Bookmarked %s → %s", b.Name, b.Key), false
	}
}

func (m *model) removeBookmark(i int) {
	if i >= len(m.bookmarks) {
		return
	}
	name := m.bookmarks[i].Name
	if m.setBookmarks(slices.Delete(slices.Clone(m.cfg.Bookmarks), i, i+1)) {
		m.sidebarCursor = clamp(m.sidebarCursor, 0, len(m.bookmarks)-1)
		m.sidebarFocus = len(m.bookmarks) > 0
		m.status, m.statusIsErr = "Removed bookmark "+name, false
	}
}

func (m *model) setBookmarks(paths []string) bool {
	if err := saveBookmarks(paths); err != nil {
		m.setErr(fmt.Errorf("saving bookmarks: %w", err))
		return false
	}
	m.cfg.Bookmarks = paths
	m.bindings, m.bookmarks, _ = buildBindings(m.cfg)
	m.cfgModTime = configModTime() // our own write isn't an external change to reload
	return true
}

// reloadConfigIfChanged applies an edited config.toml: bookmarks, keys, commands, icons, theme, git.
// Runtime toggles (hidden files, sidebar, sort) keep their current state.
func (m *model) reloadConfigIfChanged() {
	mod := configModTime()
	if mod.Equal(m.cfgModTime) {
		return
	}
	m.cfgModTime = mod
	cfg, warn := loadConfig()
	bindings, bookmarks, err := buildBindings(cfg)
	m.cfg, m.bindings, m.bookmarks = cfg, bindings, bookmarks
	m.sidebarCursor = clamp(m.sidebarCursor, 0, len(bookmarks)-1)
	m.preview.theme, m.preview.icons = cfg.SyntaxTheme, cfg.Icons
	m.preview.refresh()
	m.git.enabled = cfg.Git
	if err := errors.Join(warn, err); err != nil {
		m.setErr(err)
		return
	}
	m.status, m.statusIsErr = "Config reloaded", false
}

// choose writes the targets to the picker file and quits (picker mode, e.g. from nvim).
func (m *model) choose() tea.Cmd {
	paths := m.targets()
	if err := os.WriteFile(m.chooseFile, []byte(strings.Join(paths, "\n")+"\n"), 0o600); err != nil {
		m.setErr(fmt.Errorf("writing %s: %w", m.chooseFile, err))
		return nil
	}
	return tea.Quit
}

func (m *model) zoxideAdd(path string) tea.Cmd {
	if !m.hasZoxide {
		return nil
	}
	return func() tea.Msg {
		if out, err := exec.Command("zoxide", "add", "--", path).CombinedOutput(); err != nil {
			return errMsg{fmt.Errorf("zoxide add: %v %s", err, strings.TrimSpace(string(out)))}
		}
		return nil
	}
}

type editorDoneMsg struct{ err error }

func editorCommand() []string {
	return strings.Fields(cmp.Or(os.Getenv("VISUAL"), os.Getenv("EDITOR")))
}

// openFile opens text files in $VISUAL/$EDITOR inside the terminal, everything else with macOS `open`.
func openFile(path string) tea.Cmd {
	editor := editorCommand()
	if len(editor) > 0 && isTextFile(path) {
		cmd := exec.Command(editor[0], append(editor[1:], path)...)
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return editorDoneMsg{err} })
	}
	return func() tea.Msg {
		if out, err := exec.Command("open", path).CombinedOutput(); err != nil {
			return errMsg{fmt.Errorf("open: %s", strings.TrimSpace(string(out)))}
		}
		return nil
	}
}

func (m *model) setErr(err error) {
	m.status, m.statusIsErr = strings.ReplaceAll(err.Error(), "\n", "; "), true
}

func (m *model) bodyHeight() int { return max(m.height-2, 1) }

// ---- view ----

func (m *model) View() string {
	if m.width < 20 || m.height < 5 {
		return "window too small"
	}
	h := m.bodyHeight()
	var body string
	switch {
	case m.finder != nil:
		body = m.finder.view(m.width, h)
	case m.gotoPanel != nil:
		body = m.gotoPanel.view(m.width, h)
	case m.help != nil:
		body = m.help.view(m.width, h)
	default:
		body = m.bodyView(h)
		// Drawn over the bottom of the body instead of shrinking it, so the preview doesn't reload.
		if hints := m.hintLines(h); len(hints) > 0 {
			lines := strings.Split(body, "\n")
			copy(lines[len(lines)-len(hints):], hints)
			body = strings.Join(lines, "\n")
		}
		if box := m.dialogView(); box != "" {
			body = overlayCenter(body, box, m.width)
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.breadcrumbView(), body, m.statusView())
}

// hintLines is the which-key panel: every binding that continues the pending key sequence.
func (m *model) hintLines(maxLines int) []string {
	if m.pending == "" {
		return nil
	}
	type hint struct{ key, desc string }
	var hints []hint
	cellW := 0
	for k, action := range m.bindings {
		if rest, ok := strings.CutPrefix(k, m.pending); ok && rest != "" && !isNamedKey(k) {
			h := hint{rest, m.actionDesc(action)}
			hints = append(hints, h)
			cellW = max(cellW, runewidth.StringWidth(h.key+h.desc)+4)
		}
	}
	if len(hints) == 0 {
		return nil
	}
	slices.SortFunc(hints, func(a, b hint) int { return strings.Compare(a.key, b.key) })
	cellW = min(cellW, m.width)
	perRow := max(m.width/cellW, 1)

	lines := []string{dimStyle.Render(strings.Repeat("─", m.width))}
	for i := 0; i < len(hints) && len(lines) < maxLines; i += perRow {
		var row strings.Builder
		for _, h := range hints[i:min(i+perRow, len(hints))] {
			keyW := runewidth.StringWidth(h.key)
			row.WriteString(" " + accentStyle.Render(h.key) + " " + fileStyle.Render(fit(h.desc, cellW-keyW-2)))
		}
		lines = append(lines, row.String()+strings.Repeat(" ", max(m.width-lipgloss.Width(row.String()), 0)))
	}
	return lines
}

var namedKeys = map[string]bool{
	"up": true, "down": true, "left": true, "right": true, "enter": true, "tab": true, "space": true,
	"backspace": true, "delete": true, "esc": true, "home": true, "end": true, "pgup": true, "pgdown": true,
}

func isNamedKey(k string) bool {
	return namedKeys[k] || (len(k) > 1 && strings.Contains(k, "+")) || (len(k) > 1 && k[0] == 'f' && k[1] >= '0' && k[1] <= '9')
}

func (m *model) actionDesc(action string) string {
	if idx, ok := strings.CutPrefix(action, "bookmark:"); ok {
		i, _ := strconv.Atoi(idx)
		return "Go to " + m.bookmarks[i].Name
	}
	if idx, ok := strings.CutPrefix(action, "cmd:"); ok {
		i, _ := strconv.Atoi(idx)
		return cmp.Or(m.cfg.Commands[i].Desc, m.cfg.Commands[i].Run)
	}
	for _, a := range actions {
		if a.name == action {
			return a.desc
		}
	}
	return action
}

func (m *model) bodyView(h int) string {
	var parts []string
	if m.showSidebar {
		parts = append(parts, m.sidebarView(sidebarWidth, h), separator(h))
	}

	start, colW, available, previewW := m.layout()
	visible := m.cols[start:]
	for i, c := range visible {
		w := colW
		if i == len(visible)-1 && previewW == 0 { // last column absorbs rounding remainder
			w = available - (colW+1)*(len(visible)-1)
		}
		label := filepath.Base(c.path)
		if c.filter != "" {
			label += "  /" + c.filter
		}
		if i == 0 && start > 0 {
			label = fmt.Sprintf("‹ %d  %s", start, label)
		}
		isActive := c == m.active() && !m.sidebarFocus
		parts = append(parts, m.columnView(c, w, h, label, isActive))
		if i < len(visible)-1 || previewW > 0 {
			parts = append(parts, separator(h))
		}
	}
	if previewW > 0 {
		parts = append(parts, m.preview.view(previewW, h))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// layout returns the first visible column, the shared column width, the total width for columns
// and the preview column width (0 when there is no preview: empty folder, or no room).
func (m *model) layout() (start, colW, available, previewW int) {
	available = m.width
	if m.showSidebar {
		available -= sidebarWidth + 1
	}
	n := len(m.cols)
	if m.previewPath() != "" {
		start, colW = columnLayout(available, n+1)
		if start < n { // the active column must stay visible
			visible := n + 1 - start
			return start, colW, available, available - (colW+1)*(visible-1)
		}
	}
	start, colW = columnLayout(available, n)
	return start, colW, available, 0
}

// columnLayout returns the index of the first visible column and the width of each column.
// Columns share the space equally; when they'd get narrower than minColumnWidth, the oldest are hidden.
func columnLayout(available, n int) (start, colW int) {
	fit := max((available+1)/(minColumnWidth+1), 1)
	visible := min(n, fit)
	return n - visible, (available - (visible - 1)) / visible
}

func (m *model) columnView(c *column, w, h int, label string, isActive bool) string {
	rows := h - 1
	c.scrollTo(rows)
	visLo, visHi := -1, -1
	if m.visual != nil && m.visual.col == c {
		visLo, visHi = m.visual.bounds()
	}
	lines := []string{headerStyle.Render(fit(" "+label, w))}
	if len(c.entries) == 0 {
		lines = append(lines, dimStyle.Render(fit("  empty", w)))
	}
	for i := c.offset; i < c.offset+rows && i < len(c.entries); i++ {
		e := c.entries[i]
		text := " " + e.name
		if m.cfg.Icons {
			text = " " + entryIcon(e) + " " + e.name
		} else if e.isDir {
			text += "/"
		}
		if e.link != "" {
			text += " (→ " + tildePath(e.link) + ")"
		}
		style := fileStyle
		switch {
		case e.broken:
			style = errStyle
		case e.link != "":
			style = linkStyle.Bold(e.isDir)
		case e.isDir:
			style = dirStyle
		}
		if m.selected[filepath.Join(c.path, e.name)] {
			text = "▌" + text[1:]
			style = style.Foreground(selectColor)
		}
		if i >= visLo && i <= visHi {
			style = style.Background(visualBg)
		}
		if i == c.cursor {
			if isActive {
				style = style.Background(cursorBg).Foreground(lipgloss.Color("255"))
			} else {
				style = style.Background(inactiveBg)
			}
		}
		// Right-aligned extras: tag dots, then the git mark.
		var extra string
		extraW := 0
		if dots, n := tagDots(loadMeta(c.path, e).tags); n > 0 && w > n+6 {
			extra, extraW = dots+style.Render(" "), n+1
		}
		if mark := m.git.mark(c.path, filepath.Join(c.path, e.name)); mark != 0 && w > extraW+6 {
			extra += style.Foreground(gitColors[mark]).Render(string(mark) + " ")
			extraW += 2
		}
		lines = append(lines, style.Render(fit(text, w-extraW))+extra)
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

func (m *model) sidebarView(w, h int) string {
	lines := []string{headerStyle.Render(fit(" Bookmarks", w))}
	root := m.cols[0].path
	for i, b := range m.bookmarks {
		if len(lines) >= h {
			break
		}
		name := " " + b.Name
		if m.cfg.Icons {
			name = " " + bookmarkIcon(b.Name) + " " + b.Name
		}
		key := b.Key + " "
		text := fit(name, w-runewidth.StringWidth(key)) + dimStyle.Render(key)
		style := fileStyle
		if b.Path == root {
			style = accentStyle
		}
		if m.sidebarFocus && i == m.sidebarCursor {
			style = style.Background(cursorBg).Foreground(lipgloss.Color("255"))
			text = fit(name, w-runewidth.StringWidth(key)) + key
		}
		lines = append(lines, style.Render(text))
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

func (m *model) breadcrumbView() string {
	path := tildePath(m.active().path)
	tabs := m.tabBar()
	avail := m.width - runewidth.StringWidth(m.pending) - lipgloss.Width(tabs) - 3
	if runewidth.StringWidth(path) > avail {
		path = "…" + truncateLeft(path, avail-1)
	}
	dir, base := filepath.Split(path)
	left := " " + dimStyle.Render(dir) + accentStyle.Render(base)
	return spread(left, accentStyle.Render(m.pending)+" "+tabs, m.width)
}

func (m *model) statusView() string {
	if p := m.prompt; p != nil && p.inline {
		title := " " + p.title + ": "
		p.input.Width = max(m.width-runewidth.StringWidth(title)-2, 1)
		return accentStyle.Render(title) + p.input.View()
	}
	var left string
	if m.status != "" {
		style := fileStyle
		if m.statusIsErr {
			style = errStyle
		}
		left = style.Render(" " + runewidth.Truncate(m.status, m.width-10, "…"))
	} else {
		c := m.active()
		left = dimStyle.Render(fmt.Sprintf(" %d/%d", min(c.cursor+1, len(c.entries)), len(c.entries)))
		if e, ok := c.selected(); ok && !m.sidebarFocus {
			left += dimStyle.Render("  " + m.entryInfo(c, e))
		}
		var flags []string
		if m.showHidden {
			flags = append(flags, "hidden")
		}
		if m.sortBy != "name" || m.sortReverse {
			flags = append(flags, "sort:"+m.sortBy+map[bool]string{true: "↑"}[m.sortReverse])
		}
		if len(flags) > 0 {
			left += dimStyle.Render("  " + strings.Join(flags, " "))
		}
	}
	if m.visual != nil {
		lo, hi := m.visual.bounds()
		left += accentStyle.Render(fmt.Sprintf("  VISUAL %d", hi-lo+1))
	}
	if n := len(m.selected); n > 0 {
		left += accentStyle.Render(fmt.Sprintf("  %d selected", n))
	}
	if n := len(m.clip.paths); n > 0 {
		verb := "copied"
		if m.clip.cut {
			verb = "cut"
		}
		left += dimStyle.Render(fmt.Sprintf("  %d %s", n, verb))
	}
	return spread(left, m.jobView()+dimStyle.Render("? help "), m.width)
}

// jobView shows the oldest running job: "Copying 3 items ▰▰▰▱▱▱ 45% 1.2 GB/2.6 GB".
func (m *model) jobView() string {
	if len(m.jobs) == 0 {
		return ""
	}
	j := m.jobs[0]
	text := j.label
	if total := j.total.Load(); total > 0 {
		done := min(j.done.Load(), total)
		const barW = 10
		filled := int(done * barW / total)
		text += fmt.Sprintf(" %s%s %d%% %s/%s", strings.Repeat("▰", filled), strings.Repeat("▱", barW-filled),
			done*100/total, humanSize(done), humanSize(total))
	} else {
		text += "…"
	}
	if len(m.jobs) > 1 {
		text += fmt.Sprintf(" +%d", len(m.jobs)-1)
	}
	return accentStyle.Render(text) + "  "
}

// entryInfo is the info line for the entry under the cursor: size, modified time, permissions, link target.
func (m *model) entryInfo(c *column, e entry) string {
	meta := loadMeta(c.path, e)
	parts := []string{meta.mode.String(), meta.modTime.Format("2006-01-02 15:04")}
	if size, ok := m.dirSizes[filepath.Join(c.path, e.name)]; ok && e.isDir {
		parts = append([]string{humanSize(size)}, parts...)
	} else if !e.isDir {
		parts = append([]string{humanSize(meta.size)}, parts...)
	}
	if len(meta.tags) > 0 {
		parts = append(parts, "tags: "+strings.Join(tagNames(meta.tags), ", "))
	}
	if meta.mode&fs.ModeSymlink != 0 {
		if target, err := os.Readlink(filepath.Join(c.path, e.name)); err == nil {
			parts = append(parts, "→ "+tildePath(target))
		}
	}
	return strings.Join(parts, "  ")
}

// ---- helpers ----

func separator(h int) string {
	return dimStyle.Render(strings.TrimSuffix(strings.Repeat("│\n", h), "\n"))
}

// fit truncates or pads plain text to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.FillRight(runewidth.Truncate(s, w, "…"), w)
}

func truncateLeft(s string, w int) string {
	r := []rune(s)
	for len(r) > 0 && runewidth.StringWidth(string(r)) > w {
		r = r[1:]
	}
	return string(r)
}

// spread places left and right (styled strings) on one line of width w.
func spread(left, right string, w int) string {
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		return lo
	}
	return max(lo, min(v, hi))
}
