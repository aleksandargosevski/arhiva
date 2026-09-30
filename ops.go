package main

import (
	"archive/zip"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type clipboard struct {
	paths []string
	cut   bool
}

// prompt is a one-line input shown in place of the status line.
type prompt struct {
	title  string
	input  textinput.Model
	submit func(string) tea.Cmd
	change func(string) // optional, called on every edit
	cancel func()       // optional, called on esc
	inline bool         // shown in the status line instead of a centered popup (live filter)
}

// opDoneMsg reports a finished background file operation.
type opDoneMsg struct {
	status     string
	err        error
	selectName string
	undo       *undoStep
	job        *job // finished job to remove from the status line
}

// targets are the paths an operation acts on: selection plus visual range, or the entry under the cursor.
func (m *model) targets() []string {
	set := maps.Clone(m.selected)
	col := m.active()
	if m.visual != nil {
		lo, hi := m.visual.bounds()
		for _, e := range col.entries[lo : hi+1] {
			set[filepath.Join(col.path, e.name)] = true
		}
	}
	if len(set) == 0 {
		if e, ok := col.selected(); ok {
			return []string{filepath.Join(col.path, e.name)}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func (m *model) clearSelection() {
	clear(m.selected)
	m.visual = nil
}

// ---- copy / cut / paste ----

func (m *model) copyToClipboard(cut bool) {
	paths := m.targets()
	if len(paths) == 0 {
		return
	}
	m.clip = clipboard{paths, cut}
	m.clearSelection()
	verb := "Copied"
	if cut {
		verb = "Cut"
	}
	m.status, m.statusIsErr = fmt.Sprintf("%s %s · p to paste", verb, countLabel(paths)), false
}

func (m *model) paste() tea.Cmd {
	clip, dir := m.clip, m.active().path
	if len(clip.paths) == 0 {
		m.setErr(errors.New("nothing to paste, copy (y) or cut (x) first"))
		return nil
	}
	if clip.cut {
		m.clip = clipboard{}
	}
	verb := "Copying"
	if clip.cut {
		verb = "Moving"
	}
	j := &job{label: verb + " " + countLabel(clip.paths)}
	m.jobs = append(m.jobs, j)
	return func() tea.Msg {
		msg := pasteAll(clip, dir, j)
		msg.job = j
		return msg
	}
}

// pasteAll copies or moves the clipboard into dir, renaming to "name (1).ext" on conflict.
// Moves are plain renames; only copies and moves across volumes go through cp, with progress.
func pasteAll(clip clipboard, dir string, j *job) opDoneMsg {
	type pending struct{ src, dest string }
	var toCopy []pending
	var errs []error
	var last string
	taken := map[string]bool{}
	undo := &undoStep{desc: "paste"}
	fail := func(src string, err error) { errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(src), err)) }

	for _, src := range clip.paths {
		if clip.cut && filepath.Dir(src) == dir {
			last = filepath.Base(src)
			continue
		}
		if dir == src || strings.HasPrefix(dir, src+string(filepath.Separator)) {
			fail(src, errors.New("cannot paste a folder into itself"))
			continue
		}
		if _, err := os.Lstat(src); err != nil {
			fail(src, err)
			continue
		}
		dest := uniquePath(dir, filepath.Base(src), taken)
		taken[dest] = true
		if clip.cut {
			err := os.Rename(src, dest)
			if err == nil {
				undo.moves = append(undo.moves, move{src, dest})
				last = filepath.Base(dest)
				continue
			}
			if !errors.Is(err, syscall.EXDEV) { // EXDEV: other volume, copy + delete below
				fail(src, err)
				continue
			}
		}
		toCopy = append(toCopy, pending{src, dest})
	}

	for _, p := range toCopy {
		j.total.Add(treeSize(p.src))
	}
	for _, p := range toCopy {
		if err := copyWithProgress(p.src, p.dest, j); err != nil {
			fail(p.src, err)
			continue
		}
		last = filepath.Base(p.dest)
		if !clip.cut {
			undo.created = append(undo.created, p.dest)
			continue
		}
		undo.moves = append(undo.moves, move{p.src, p.dest})
		if err := os.RemoveAll(p.src); err != nil {
			fail(p.src, fmt.Errorf("copied, but removing the original failed: %w", err))
		}
	}

	verb := "Copied"
	if clip.cut {
		verb = "Moved"
	}
	return opDoneMsg{
		status:     fmt.Sprintf("%s %d item(s) to %s", verb, len(clip.paths)-len(errs), tildePath(dir)),
		err:        errors.Join(errs...),
		selectName: last,
		undo:       undo,
	}
}

// job is a running background operation whose progress is shown in the status line.
type job struct {
	label       string
	total, done atomic.Int64 // bytes
}

// copyWithProgress runs `cp -Rp` (keeps attributes, uses APFS clones) and measures progress
// by summing what already arrived at dest.
func copyWithProgress(src, dest string, j *job) error {
	base := j.done.Load()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				j.done.Store(base + treeSize(dest))
			}
		}
	}()
	err := runCmd("cp", "-Rp", "--", src, dest)
	close(stop)
	wg.Wait()
	j.done.Store(base + treeSize(src))
	return err
}

// treeSize sums regular file sizes under path; unreadable parts count as zero.
func treeSize(path string) int64 {
	var size int64
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

func moveFile(src, dest string) error {
	err := os.Rename(src, dest)
	if errors.Is(err, syscall.EXDEV) { // different volume
		err = runCmd("mv", "--", src, dest)
	}
	return err
}

// uniquePath returns dir/name, or "name (1).ext" etc. if that exists or is in taken.
func uniquePath(dir, name string, taken map[string]bool) string {
	stem, ext := splitExt(name)
	path := filepath.Join(dir, name)
	for i := 1; exists(path) || taken[path]; i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
	return path
}

// ---- new / rename ----

func (m *model) startNew() tea.Cmd {
	return m.openPrompt("New (end with / for folder)", "", 0, m.newEntry)
}

// newEntry creates a file, or a folder when name ends with "/". Nested paths like "a/b/c.txt" are allowed.
func (m *model) newEntry(name string) tea.Cmd {
	if name == "" {
		return nil
	}
	rel := filepath.Clean(name)
	if filepath.IsAbs(name) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		m.setErr(fmt.Errorf("invalid name %q: must stay inside the current folder", name))
		return nil
	}
	path := filepath.Join(m.active().path, rel)
	if exists(path) {
		m.setErr(fmt.Errorf("%s already exists", rel))
		return nil
	}
	isDir := strings.HasSuffix(name, "/")
	created := topmostMissing(m.active().path, rel)
	var err error
	if isDir {
		err = os.MkdirAll(path, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		var f *os.File
		if f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		m.setErr(err)
		return nil
	}
	first, _, _ := strings.Cut(rel, "/")
	if strings.HasPrefix(first, ".") {
		m.showHidden = true
	}
	m.afterOp(first)
	m.pushUndo(&undoStep{desc: "create " + rel, created: []string{created}})
	m.status, m.statusIsErr = "Created "+rel, false
	return nil
}

// topmostMissing returns the first path component of dir/rel that doesn't exist yet: the one
// that has to be removed to undo creating rel.
func topmostMissing(dir, rel string) string {
	path := dir
	for _, part := range strings.Split(rel, "/") {
		path = filepath.Join(path, part)
		if !exists(path) {
			return path
		}
	}
	return path
}

// startRename opens the rename prompt. With several targets it becomes a bulk rename in $EDITOR. mode: "rename" (cursor before extension),
// "rename_append" (cursor at end), "rename_replace" (only the extension kept, cursor before it).
func (m *model) startRename(mode string) tea.Cmd {
	if paths := m.targets(); len(paths) > 1 {
		return m.startBulkRename(paths)
	}
	col := m.active()
	e, ok := col.selected()
	if !ok {
		return nil
	}
	stem, ext := e.name, ""
	if !e.isDir {
		stem, ext = splitExt(e.name)
	}
	value, cursor := e.name, len([]rune(stem))
	switch mode {
	case "rename_append":
		cursor = len([]rune(e.name))
	case "rename_replace":
		value, cursor = ext, 0
	}
	old := filepath.Join(col.path, e.name)
	return m.openPrompt("Rename", value, cursor, func(name string) tea.Cmd {
		m.rename(old, name)
		return nil
	})
}

func (m *model) rename(old, name string) {
	oldName := filepath.Base(old)
	if name == "" || name == oldName {
		return
	}
	if strings.ContainsRune(name, '/') || name == "." || name == ".." {
		m.setErr(fmt.Errorf("invalid name %q", name))
		return
	}
	dest := filepath.Join(filepath.Dir(old), name)
	// Case-only renames are fine on case-insensitive filesystems.
	if exists(dest) && !strings.EqualFold(name, oldName) {
		m.setErr(fmt.Errorf("%s already exists", name))
		return
	}
	if err := os.Rename(old, dest); err != nil {
		m.setErr(err)
		return
	}
	if m.selected[old] {
		delete(m.selected, old)
		m.selected[dest] = true
	}
	m.pushUndo(&undoStep{desc: "rename " + oldName, moves: []move{{old, dest}}})
	if strings.HasPrefix(name, ".") {
		m.showHidden = true
	}
	m.afterOp(name)
	m.status, m.statusIsErr = fmt.Sprintf("Renamed %s → %s", oldName, name), false
}

// ---- zip ----

func (m *model) startZip() tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	m.visual = nil
	name := "Archive.zip"
	if len(paths) == 1 {
		name = filepath.Base(paths[0]) + ".zip"
	}
	dir := m.active().path
	return m.openPrompt("Zip "+countLabel(paths)+" as", name, len([]rune(name))-len(".zip"), func(name string) tea.Cmd {
		if name == "" {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(name), ".zip") {
			name += ".zip"
		}
		dest := filepath.Join(dir, name)
		if strings.ContainsRune(name, '/') {
			m.setErr(fmt.Errorf("invalid name %q", name))
			return nil
		}
		if exists(dest) {
			m.setErr(fmt.Errorf("%s already exists", name))
			return nil
		}
		m.clearSelection()
		m.status, m.statusIsErr = "Zipping…", false
		return func() tea.Msg {
			err := zipPaths(dest, paths)
			return opDoneMsg{status: "Created " + name, err: err, selectName: name, undo: &undoStep{desc: "zip", created: []string{dest}}}
		}
	})
}

// zipPaths writes each path (recursively) into a new zip at dest, named relative to its parent folder.
// ponytail: symlinks and special files are skipped; add them if someone zips a tree that relies on them.
func zipPaths(dest string, paths []string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".arhiva-*.zip")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	zw := zip.NewWriter(tmp)
	for _, root := range paths {
		base := filepath.Dir(root)
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == tmp.Name() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, path)
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
				_, err = zw.CreateHeader(hdr)
				return err
			}
			hdr.Method = zip.Deflate
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		})
		if err != nil {
			return err
		}
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = tmp.Chmod(0o644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// ---- prompt ----

func (m *model) openPrompt(title, value string, cursor int, submit func(string) tea.Cmd) tea.Cmd {
	input := textinput.New()
	input.Prompt = ""
	input.SetValue(value)
	input.SetCursor(cursor)
	input.Focus()
	m.prompt = &prompt{title: title, input: input, submit: submit}
	return textinput.Blink
}

func (m *model) updatePrompt(msg tea.KeyMsg) tea.Cmd {
	p := m.prompt
	switch msg.String() {
	case "esc":
		m.prompt = nil
		if p.cancel != nil {
			p.cancel()
		}
	case "enter":
		m.prompt = nil
		return p.submit(strings.TrimSpace(p.input.Value()))
	default:
		before := p.input.Value()
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(msg)
		if p.change != nil && p.input.Value() != before {
			p.change(p.input.Value())
		}
		return cmd
	}
	return nil
}

// afterOp reloads the columns after a file operation and puts the cursor on selectName if given.
func (m *model) afterOp(selectName string) {
	m.reloadColumns()
	if selectName != "" {
		m.active().selectName(selectName)
	}
	m.preview.refresh()
}

// ---- helpers ----

// splitExt splits "a.tar.gz" into "a.tar", ".gz"; dotfiles like ".zshrc" have no extension.
func splitExt(name string) (stem, ext string) {
	ext = filepath.Ext(name)
	if ext == name {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

func countLabel(paths []string) string {
	if len(paths) == 1 {
		return filepath.Base(paths[0])
	}
	return fmt.Sprintf("%d items", len(paths))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", name, cmp.Or(strings.TrimSpace(string(out)), err.Error()))
	}
	return nil
}

// ---- delete ----

// confirm is a y/N question shown in the status line; any key other than y cancels.
type confirm struct {
	question string
	details  []string // e.g. the names about to be deleted
	yes      func() tea.Cmd
}

func (m *model) updateConfirm(msg tea.KeyMsg) tea.Cmd {
	c := m.confirm
	m.confirm = nil
	if msg.String() == "y" || msg.String() == "Y" {
		return c.yes()
	}
	m.status, m.statusIsErr = "Cancelled", false
	return nil
}

// startDelete asks for confirmation, then moves targets to the Trash or removes them for good.
func (m *model) startDelete(permanent bool) {
	paths := m.targets()
	if len(paths) == 0 {
		return
	}
	for _, p := range paths {
		if p == "/" || p == homeDir() {
			m.setErr(fmt.Errorf("refusing to delete %s", p))
			return
		}
	}
	question := "Move " + countLabel(paths) + " to Trash?"
	if permanent {
		question = "Permanently delete " + countLabel(paths) + "? This cannot be undone."
	}
	var details []string
	for _, p := range paths {
		details = append(details, tildePath(p))
	}
	m.confirm = &confirm{question: question, details: details, yes: func() tea.Cmd {
		m.clearSelection()
		m.clip.paths = slices.DeleteFunc(m.clip.paths, func(p string) bool { return slices.Contains(paths, p) })
		m.status, m.statusIsErr = "Deleting…", false
		return func() tea.Msg {
			if permanent {
				var errs []error
				for _, p := range paths {
					if err := os.RemoveAll(p); err != nil {
						errs = append(errs, err)
					}
				}
				return opDoneMsg{status: "Deleted " + countLabel(paths), err: errors.Join(errs...)}
			}
			return opDoneMsg{status: "Moved " + countLabel(paths) + " to Trash", err: moveToTrash(paths)}
		}
	}}
}

func moveToTrash(paths []string) error {
	if _, err := exec.LookPath("trash"); err != nil {
		return errors.New("trash command not found (needs macOS 15+); use D to delete permanently")
	}
	return runCmd("trash", paths...)
}

// ---- extract ----

// Formats bsdtar (libarchive) can unpack; longest suffixes first so ".tar.gz" wins over ".gz".
var archiveExts = []string{
	".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tar.lz4", ".tar.lzma",
	".zip", ".tar", ".tgz", ".tbz", ".tbz2", ".txz", ".7z", ".rar", ".iso", ".cpio", ".xar", ".jar", ".cab", ".lha", ".lzh",
}

// Single-file compressors, not archives: decompressed to one file.
var compressors = map[string][]string{".gz": {"gzip", "-dc"}, ".bz2": {"bzip2", "-dc"}, ".xz": {"xz", "-dc"}, ".zst": {"zstd", "-dc"}}

func archiveExt(path string) string {
	lower := strings.ToLower(path)
	for _, ext := range archiveExts {
		if strings.HasSuffix(lower, ext) {
			return ext
		}
	}
	if ext := filepath.Ext(lower); compressors[ext] != nil {
		return ext
	}
	return ""
}

func isArchive(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && archiveExt(path) != ""
}

func (m *model) extract(paths []string) tea.Cmd {
	dir := m.active().path
	m.clearSelection()
	m.status, m.statusIsErr = "Extracting…", false
	return func() tea.Msg {
		var errs []error
		var last string
		undo := &undoStep{desc: "extract"}
		for _, p := range paths {
			name, err := extractOne(p, dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(p), err))
				continue
			}
			last = name
			undo.created = append(undo.created, filepath.Join(dir, name))
		}
		return opDoneMsg{status: "Extracted " + countLabel(paths), err: errors.Join(errs...), selectName: last, undo: undo}
	}
}

// extractOne unpacks archive into dir. If the archive has a single top-level entry it lands directly
// in dir, otherwise everything goes into a folder named after the archive. Returns the created name.
func extractOne(archive, dir string) (string, error) {
	ext := archiveExt(archive)
	base := filepath.Base(archive)
	stem := base[:len(base)-len(ext)]

	if cmd := compressors[ext]; cmd != nil {
		return stem, decompress(cmd, archive, uniquePath(dir, stem, nil))
	}

	tmp, err := os.MkdirTemp(dir, ".arhiva-extract-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := runCmd("tar", "-xf", archive, "-C", tmp); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", err
	}
	src, name := tmp, stem
	if len(entries) == 1 {
		src, name = filepath.Join(tmp, entries[0].Name()), entries[0].Name()
	}
	dest := uniquePath(dir, name, nil)
	if src == tmp {
		if err := os.Chmod(tmp, 0o755); err != nil { // MkdirTemp creates 0700
			return "", err
		}
	}
	return filepath.Base(dest), os.Rename(src, dest)
}

func decompress(cmd []string, src, dest string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var stderr strings.Builder
	c := exec.Command(cmd[0], append(cmd[1:], src)...)
	c.Stdout, c.Stderr = out, &stderr
	err = c.Run()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("%s: %s", cmd[0], cmp.Or(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return nil
}

func all[T any](items []T, pred func(T) bool) bool {
	for _, it := range items {
		if !pred(it) {
			return false
		}
	}
	return true
}

// ---- bulk rename ----

// startBulkRename opens the names in $EDITOR, one per line; saved lines become the new names.
func (m *model) startBulkRename(paths []string) tea.Cmd {
	editor := editorCommand()
	if len(editor) == 0 {
		editor = []string{"vi"}
	}
	var names []string
	for _, p := range paths {
		if strings.ContainsRune(filepath.Base(p), '\n') {
			m.setErr(fmt.Errorf("cannot bulk rename %q: name contains a newline", filepath.Base(p)))
			return nil
		}
		names = append(names, filepath.Base(p))
	}
	f, err := os.CreateTemp("", "arhiva-rename-*.txt")
	if err == nil {
		_, err = f.WriteString(strings.Join(names, "\n") + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		m.setErr(fmt.Errorf("bulk rename: %w", err))
		return nil
	}
	m.clearSelection()
	cmd := exec.Command(editor[0], append(editor[1:], f.Name())...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer os.Remove(f.Name())
		if err != nil {
			return opDoneMsg{err: fmt.Errorf("editor: %w", err)}
		}
		data, err := os.ReadFile(f.Name())
		if err != nil {
			return opDoneMsg{err: err}
		}
		lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
		undo, err := bulkRename(paths, lines)
		n := 0
		if undo != nil {
			n = len(undo.moves)
		}
		return opDoneMsg{status: fmt.Sprintf("Renamed %d item(s)", n), err: err, undo: undo}
	})
}

// bulkRename renames paths[i] to names[i] within its own folder. Everything is validated first,
// then renamed in two phases through temporary names, so swaps like a→b, b→a work.
func bulkRename(paths, names []string) (*undoStep, error) {
	if len(names) != len(paths) {
		return nil, fmt.Errorf("line count changed (%d → %d), nothing renamed", len(paths), len(names))
	}
	var moves []move
	moving := map[string]bool{}
	for i, src := range paths {
		name := names[i]
		if name == filepath.Base(src) {
			continue
		}
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
			return nil, fmt.Errorf("invalid name %q on line %d, nothing renamed", name, i+1)
		}
		moves = append(moves, move{src, filepath.Join(filepath.Dir(src), name)})
		moving[src] = true
	}
	seen := map[string]bool{}
	for _, mv := range moves {
		key := strings.ToLower(mv.to) // default macOS volumes are case-insensitive
		if seen[key] {
			return nil, fmt.Errorf("%s used twice, nothing renamed", filepath.Base(mv.to))
		}
		seen[key] = true
		if exists(mv.to) && !moving[mv.to] && !strings.EqualFold(mv.to, mv.from) {
			return nil, fmt.Errorf("%s already exists, nothing renamed", filepath.Base(mv.to))
		}
	}

	tmps := make([]string, len(moves))
	for i, mv := range moves {
		tmps[i] = filepath.Join(filepath.Dir(mv.from), fmt.Sprintf(".arhiva-rename-%d-%s", i, filepath.Base(mv.from)))
		if err := os.Rename(mv.from, tmps[i]); err != nil {
			for j := range i { // put back what was already moved
				os.Rename(tmps[j], moves[j].from)
			}
			return nil, err
		}
	}
	done := &undoStep{desc: "bulk rename"}
	var errs []error
	for i, mv := range moves {
		if err := os.Rename(tmps[i], mv.to); err != nil {
			errs = append(errs, err)
			os.Rename(tmps[i], mv.from)
			continue
		}
		done.moves = append(done.moves, mv)
	}
	return done, errors.Join(errs...)
}

// ---- quick look / size ----

// quickLookPanel opens the macOS Quick Look panel; it runs until the panel is closed.
func quickLookPanel(paths []string) tea.Cmd {
	return func() tea.Msg {
		if err := exec.Command("qlmanage", append([]string{"-p"}, paths...)...).Run(); err != nil {
			return errMsg{fmt.Errorf("quick look: %w", err)}
		}
		return nil
	}
}

type dirSizeMsg struct {
	sizes  map[string]int64
	status string
}

// calcSizes sums the sizes of regular files under each path (logical size, like Finder).
func calcSizes(paths []string) tea.Cmd {
	return func() tea.Msg {
		sizes := map[string]int64{}
		var total int64
		for _, root := range paths {
			sizes[root] = treeSize(root)
			total += sizes[root]
		}
		return dirSizeMsg{sizes, fmt.Sprintf("%s: %s", countLabel(paths), humanSize(total))}
	}
}

// ---- symlink / diff ----

// symlink creates links in the current folder pointing at the copied/cut paths; the clipboard stays.
func (m *model) symlink() {
	dir := m.active().path
	if len(m.clip.paths) == 0 {
		m.setErr(errors.New("nothing to link, copy (y) something first"))
		return
	}
	undo := &undoStep{desc: "symlink"}
	var errs []error
	var last string
	taken := map[string]bool{}
	for _, target := range m.clip.paths {
		dest := uniquePath(dir, filepath.Base(target), taken)
		taken[dest] = true
		if err := os.Symlink(target, dest); err != nil {
			errs = append(errs, err)
			continue
		}
		undo.created = append(undo.created, dest)
		last = filepath.Base(dest)
	}
	m.afterOp(last)
	m.pushUndo(undo)
	if err := errors.Join(errs...); err != nil {
		m.setErr(err)
		return
	}
	m.status, m.statusIsErr = fmt.Sprintf("Linked %s", countLabel(m.clip.paths)), false
}

// diff compares exactly two targets: in (n)vim's diff mode when that's the editor and both are
// files, otherwise with `git diff --no-index` in less.
func (m *model) diff() tea.Cmd {
	paths := m.targets()
	if len(paths) != 2 {
		m.setErr(fmt.Errorf("diff needs exactly 2 selected items, got %d", len(paths)))
		return nil
	}
	a, b := paths[0], paths[1]
	editor := editorCommand()
	var cmd *exec.Cmd
	if len(editor) > 0 && strings.Contains(filepath.Base(editor[0]), "vim") && isRegular(a) && isRegular(b) {
		cmd = exec.Command(editor[0], append(editor[1:], "-d", a, b)...)
	} else {
		cmd = exec.Command("sh", "-c", `git diff --no-index --color=always -- "$1" "$2" | less -R`, "arhiva", a, b)
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return errMsg{fmt.Errorf("diff: %w", err)}
		}
		return nil
	})
}

func isRegular(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ---- duplicate / copy contents ----

// duplicate copies each target next to itself as "name (1).ext".
func (m *model) duplicate() tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	m.clearSelection()
	byDir := map[string][]string{}
	for _, p := range paths {
		byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], p)
	}
	activeDir := m.active().path
	j := &job{label: "Duplicating " + countLabel(paths)}
	m.jobs = append(m.jobs, j)
	return func() tea.Msg {
		done := opDoneMsg{status: "Duplicated " + countLabel(paths), undo: &undoStep{desc: "duplicate"}, job: j}
		var errs []error
		for dir, group := range byDir {
			r := pasteAll(clipboard{paths: group}, dir, j)
			errs = append(errs, r.err)
			done.undo.created = append(done.undo.created, r.undo.created...)
			if dir == activeDir {
				done.selectName = r.selectName
			}
		}
		done.err = errors.Join(errs...)
		return done
	}
}

// copyContents puts a single text file's contents on the clipboard; anything else is copied as
// file references, which paste as attachments in Slack, Mail, Messages and as files in Finder.
func (m *model) copyContents() tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	return func() tea.Msg {
		if len(paths) == 1 && isTextFile(paths[0]) {
			f, err := os.Open(paths[0])
			if err != nil {
				return errMsg{err}
			}
			defer f.Close()
			cmd := exec.Command("pbcopy")
			cmd.Stdin = f
			if err := cmd.Run(); err != nil {
				return errMsg{fmt.Errorf("pbcopy: %w", err)}
			}
			return opDoneMsg{status: "Copied contents of " + countLabel(paths)}
		}
		const script = `function run(paths) {
			ObjC.import("AppKit");
			const pb = $.NSPasteboard.generalPasteboard;
			pb.clearContents;
			if (!pb.writeObjects($(paths.map(p => $.NSURL.fileURLWithPath(p))))) throw new Error("pasteboard refused");
		}`
		if err := runCmd("osascript", append([]string{"-l", "JavaScript", "-e", script}, paths...)...); err != nil {
			return errMsg{err}
		}
		return opDoneMsg{status: "Copied " + countLabel(paths) + " as file(s)"}
	}
}

// ---- airdrop ----

// airDropScript opens the system AirDrop panel for the given files and waits until it is closed,
// since osascript exiting would close the panel. Cancelling is not an error.
const airDropScript = `function run(paths) {
  ObjC.import("AppKit");
  const app = $.NSApplication.sharedApplication;
  app.setActivationPolicy($.NSApplicationActivationPolicyAccessory);
  const svc = $.NSSharingService.sharingServiceNamed($.NSSharingServiceNameSendViaAirDrop);
  if (!svc || svc.isNil()) throw new Error("AirDrop is not available");
  const items = $(paths.map(p => $.NSURL.fileURLWithPath(p)));
  if (!svc.canPerformWithItems(items)) throw new Error("AirDrop can't share these items (are Wi-Fi and Bluetooth on?)");
  let done = false, failed = null;
  ObjC.registerSubclass({
    name: "ArhivaAirDropDelegate",
    protocols: ["NSSharingServiceDelegate"],
    methods: {
      "sharingService:didShareItems:": { types: ["void", ["id", "id"]], implementation: () => { done = true; } },
      "sharingService:didFailToShareItems:error:": { types: ["void", ["id", "id", "id"]], implementation: (s, i, e) => { failed = e.localizedDescription.js; done = true; } },
    },
  });
  svc.delegate = $.ArhivaAirDropDelegate.alloc.init;
  app.activateIgnoringOtherApps(true);
  svc.performWithItems(items);
  const deadline = Date.now() + 10 * 60 * 1000;
  while (!done && Date.now() < deadline) {
    $.NSRunLoop.currentRunLoop.runUntilDate($.NSDate.dateWithTimeIntervalSinceNow(0.2));
  }
  if (failed && !/cancel/i.test(failed)) throw new Error(failed);
  return failed ? "cancelled" : "sent";
}`

func (m *model) airDrop() tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	m.clearSelection()
	m.status, m.statusIsErr = "AirDrop: pick a device in the panel…", false
	return func() tea.Msg {
		out, err := exec.Command("osascript", append([]string{"-l", "JavaScript", "-e", airDropScript}, paths...)...).CombinedOutput()
		result := strings.TrimSpace(string(out))
		if err != nil {
			// osascript prints "execution error: Error: <message> (-2700)"
			if _, msg, ok := strings.Cut(result, "Error: "); ok {
				result = strings.TrimSuffix(strings.TrimSpace(msg), " (-2700)")
			}
			return errMsg{fmt.Errorf("airdrop: %s", cmp.Or(result, err.Error()))}
		}
		if result == "cancelled" {
			return opDoneMsg{status: "AirDrop cancelled"}
		}
		return opDoneMsg{status: "AirDropped " + countLabel(paths)}
	}
}
