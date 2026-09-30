package main

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFileOps(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "dst"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o644))
	m, err := newModel(Config{}, root)
	must(t, err)
	keys := func(ks ...string) {
		for _, k := range ks {
			m.handleKey(k)
		}
	}
	submit := func(value string) {
		t.Helper()
		if m.prompt == nil {
			t.Fatal("no prompt open")
		}
		p := m.prompt
		m.prompt = nil
		runCmdMsg(m, p.submit(value))
	}
	listing := func(dir string) []string {
		var names []string
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}

	keys("n")
	submit("sub/")
	keys("n")
	submit("notes/today.md")
	if !exists(filepath.Join(root, "sub")) || !exists(filepath.Join(root, "notes", "today.md")) {
		t.Fatalf("new failed: %v", listing(root))
	}
	if e, _ := m.active().selected(); e.name != "notes" {
		t.Fatalf("cursor should be on created entry, got %s", e.name)
	}

	m.active().selectName("a.txt")
	keys("y") // copy into same folder: duplicate
	runCmdMsg(m, m.paste())
	if !exists(filepath.Join(root, "a (1).txt")) {
		t.Fatalf("duplicate naming failed: %v", listing(root))
	}

	m.active().selectName("a.txt")
	keys("x")
	m.active().selectName("dst")
	keys("l")
	runCmdMsg(m, m.paste())
	if exists(filepath.Join(root, "a.txt")) || !exists(filepath.Join(root, "dst", "a.txt")) {
		t.Fatalf("move failed: root %v", listing(root))
	}

	keys("rr")
	if v, pos := m.prompt.input.Value(), m.prompt.input.Position(); v != "a.txt" || pos != 1 {
		t.Fatalf("rr: %q cursor %d", v, pos)
	}
	m.prompt = nil
	keys("rf")
	if pos := m.prompt.input.Position(); pos != 5 {
		t.Fatalf("rf cursor %d", pos)
	}
	m.prompt = nil
	keys("rn")
	if v, pos := m.prompt.input.Value(), m.prompt.input.Position(); v != ".txt" || pos != 0 {
		t.Fatalf("rn: %q cursor %d", v, pos)
	}
	submit("b.txt")
	if !slices.Equal(listing(filepath.Join(root, "dst")), []string{"b.txt"}) {
		t.Fatalf("rename failed: %v", listing(filepath.Join(root, "dst")))
	}

	keys("h", "Z") // cursor is back on dst
	submit("out")  // .zip gets appended
	zr, err := zip.OpenReader(filepath.Join(root, "out.zip"))
	must(t, err)
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		t.Fatal("empty zip")
	}
}

// runCmdMsg runs a command synchronously and feeds its message back, like Bubble Tea would.
func runCmdMsg(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		if _, ok := msg.(tea.BatchMsg); !ok {
			m.update(msg)
		}
	}
}

func TestSplitExt(t *testing.T) {
	for in, want := range map[string][2]string{"a.txt": {"a", ".txt"}, ".zshrc": {".zshrc", ""}, "a.tar.gz": {"a.tar", ".gz"}, "Makefile": {"Makefile", ""}} {
		if s, e := splitExt(in); s != want[0] || e != want[1] {
			t.Errorf("splitExt(%q) = %q %q", in, s, e)
		}
	}
}

func TestExtractAndDelete(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "src", "proj"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "src", "proj", "a.txt"), []byte("a"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "src", "b.txt"), []byte("b"), 0o644))
	src := filepath.Join(root, "src")
	must(t, zipPaths(filepath.Join(root, "one.zip"), []string{filepath.Join(src, "proj")}))
	must(t, zipPaths(filepath.Join(root, "many.zip"), []string{filepath.Join(src, "proj"), filepath.Join(src, "b.txt")}))
	must(t, runCmd("tar", "-czf", filepath.Join(root, "t.tar.gz"), "-C", src, "b.txt"))

	for archive, want := range map[string]string{"one.zip": "proj", "many.zip": "many", "t.tar.gz": "b.txt"} {
		name, err := extractOne(filepath.Join(root, archive), root)
		must(t, err)
		if name != want {
			t.Errorf("%s extracted as %q, want %q", archive, name, want)
		}
	}
	if !exists(filepath.Join(root, "proj", "a.txt")) || !exists(filepath.Join(root, "many", "proj", "a.txt")) {
		t.Fatal("extracted contents missing")
	}

	m, err := newModel(Config{}, root)
	must(t, err)
	m.active().selectName("many")
	press := func(k string) { runCmdMsg(m, m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})) }
	press("D")
	press("n") // anything but y cancels, and must not trigger "new"
	if !exists(filepath.Join(root, "many")) || m.confirm != nil || m.prompt != nil {
		t.Fatal("cancel failed")
	}
	press("D")
	press("y")
	if exists(filepath.Join(root, "many")) {
		t.Fatal("D + y should delete")
	}
}

func TestBulkRenameAndUndo(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		must(t, os.WriteFile(filepath.Join(root, n), []byte(n), 0o644))
	}
	p := func(n string) string { return filepath.Join(root, n) }
	read := func(n string) string { b, _ := os.ReadFile(p(n)); return string(b) }

	if _, err := bulkRename([]string{p("a"), p("b")}, []string{"x"}); err == nil {
		t.Fatal("line count mismatch must fail")
	}
	if _, err := bulkRename([]string{p("a")}, []string{"c"}); err == nil {
		t.Fatal("renaming onto an existing file must fail")
	}
	undo, err := bulkRename([]string{p("a"), p("b"), p("c")}, []string{"b", "a", "c"}) // swap
	must(t, err)
	if read("a") != "b" || read("b") != "a" || len(undo.moves) != 2 {
		t.Fatalf("swap failed: a=%q b=%q", read("a"), read("b"))
	}

	m, err := newModel(Config{}, root)
	must(t, err)
	m.pushUndo(undo)
	runCmdMsg(m, m.undo())
	if read("a") != "a" || read("b") != "b" {
		t.Fatalf("undo failed: %s", m.status)
	}

	m.active().selectName("c")
	m.handleKey("r")
	m.handleKey("r")
	p2 := m.prompt
	m.prompt = nil
	runCmdMsg(m, p2.submit("d"))
	runCmdMsg(m, m.undo())
	if !exists(p("c")) || exists(p("d")) {
		t.Fatal("undo single rename failed")
	}
}

func TestCustomCommand(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{`it's a "file"`, "b"} {
		must(t, os.WriteFile(filepath.Join(root, n), nil, 0o644))
	}
	cfg := Config{Commands: []Command{{Key: "X", Run: `printf '%s\n' "$@" > list; echo "cursor=$(basename "$f")"`}}}
	m, err := newModel(cfg, root)
	must(t, err)
	m.selected[filepath.Join(root, "b")] = true
	m.selected[filepath.Join(root, `it's a "file"`)] = true
	runCmdMsg(m, m.handleKey("X"))

	data, err := os.ReadFile(filepath.Join(root, "list"))
	must(t, err)
	want := filepath.Join(root, "b") + "\n" + filepath.Join(root, `it's a "file"`) + "\n"
	if string(data) != want {
		t.Fatalf("args: %q", data)
	}
	if m.status != "cursor=b" {
		t.Fatalf("status %q", m.status)
	}
	if e, _ := m.active().selected(); e.name != "b" || len(m.selected) != 0 {
		t.Fatal("columns should reload and selection clear")
	}
}

func TestPasteSameNamesAndProgress(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b", "dst"} {
		must(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(root, "a", "x.txt"), make([]byte, 1000), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "b", "x.txt"), make([]byte, 500), 0o644))

	j := &job{}
	clip := clipboard{paths: []string{filepath.Join(root, "a", "x.txt"), filepath.Join(root, "b", "x.txt")}}
	msg := pasteAll(clip, filepath.Join(root, "dst"), j)
	must(t, msg.err)
	if !exists(filepath.Join(root, "dst", "x.txt")) || !exists(filepath.Join(root, "dst", "x (1).txt")) {
		t.Fatal("same-named sources must not overwrite each other")
	}
	if j.total.Load() != 1500 || j.done.Load() != 1500 {
		t.Fatalf("progress %d/%d", j.done.Load(), j.total.Load())
	}
	if len(msg.undo.created) != 2 {
		t.Fatalf("undo should know both copies: %v", msg.undo.created)
	}
}

func TestSymlink(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "dst"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644))
	m, err := newModel(Config{}, root)
	must(t, err)
	m.clip = clipboard{paths: []string{filepath.Join(root, "a.txt")}}
	m.resetTo(filepath.Join(root, "dst"), "")
	m.symlink()
	target, err := os.Readlink(filepath.Join(root, "dst", "a.txt"))
	must(t, err)
	if target != filepath.Join(root, "a.txt") || len(m.clip.paths) != 1 {
		t.Fatalf("link → %s, clip %v", target, m.clip.paths)
	}
}

func TestTags(t *testing.T) {
	strs := []string{"Red\n6", "Work\n0", "Čćž ünïcode\n0"}
	got, err := parseBplistStrings(encodeBplistStrings(strs))
	must(t, err)
	if !slices.Equal(got, strs) {
		t.Fatalf("roundtrip: %q", got)
	}
	many := make([]string, 300) // forces 2-byte refs and long-length headers
	for i := range many {
		many[i] = strings.Repeat("x", i%40)
	}
	if got, err := parseBplistStrings(encodeBplistStrings(many)); err != nil || !slices.Equal(got, many) {
		t.Fatalf("large roundtrip failed: %v", err)
	}

	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	must(t, os.WriteFile(a, nil, 0o644))
	must(t, os.WriteFile(b, nil, 0o644))
	must(t, writeTags(a, []tag{{"Work", 0}, {"Blue", 4}}))
	must(t, writeTags(b, []tag{{"Work", 0}}))

	m, err := newModel(Config{}, root)
	must(t, err)
	m.selected[a], m.selected[b] = true, true
	m.startTags()
	if v := m.prompt.input.Value(); v != "Work" {
		t.Fatalf("prompt should hold common tags, got %q", v)
	}
	p := m.prompt
	m.prompt = nil
	p.submit("Home, Red") // remove Work from both, add Home and Red; a keeps Blue
	if got := readTags(a); fmt.Sprint(got) != "[Blue\n4 Home\n0 Red\n6]" {
		t.Fatalf("a: %q", got)
	}
	if got := readTags(b); fmt.Sprint(got) != "[Home\n0 Red\n6]" {
		t.Fatalf("b: %q", got)
	}
}

func TestDuplicateAndPicker(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644))
	m, err := newModel(Config{}, root)
	must(t, err)
	runCmdMsg(m, m.handleKey("c"))
	if !exists(filepath.Join(root, "a (1).txt")) || len(m.jobs) != 0 {
		t.Fatalf("duplicate failed: %s", m.status)
	}
	if e, _ := m.active().selected(); e.name != "a (1).txt" {
		t.Fatalf("cursor should be on the duplicate, got %s", e.name)
	}

	out := filepath.Join(t.TempDir(), "chosen")
	m.chooseFile = out
	m.active().selectName("a.txt")
	if cmd := m.handleKey("l"); cmd == nil {
		t.Fatal("choosing should quit")
	}
	data, _ := os.ReadFile(out)
	if string(data) != filepath.Join(root, "a.txt")+"\n" {
		t.Fatalf("chosen: %q", data)
	}
}

func TestConfigReload(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	m, err := newModel(Config{}, root)
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(configPath()), 0o755))
	must(t, os.WriteFile(configPath(), []byte("bookmarks = [\""+root+"\"]\n[keys]\nquit = \"x\"\n"), 0o644))
	m.reloadConfigIfChanged()
	if len(m.bookmarks) != 1 || m.bindings["x"] != "quit" || m.status != "Config reloaded" {
		t.Fatalf("reload: bookmarks %v, x=%q, status %q", m.bookmarks, m.bindings["x"], m.status)
	}
}
