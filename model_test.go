package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestColumnLayout(t *testing.T) {
	cases := []struct{ available, n, start, colW int }{
		{101, 1, 0, 101},
		{101, 2, 0, 50}, // 50 + sep + 50
		{101, 3, 0, 33},
		{101, 8, 4, 24}, // only 4 columns of >=20 fit, oldest 4 hidden
		{10, 3, 2, 10},  // tiny window still shows the active column
	}
	for _, c := range cases {
		start, colW := columnLayout(c.available, c.n)
		if start != c.start || colW != c.colW {
			t.Errorf("columnLayout(%d, %d) = %d, %d; want %d, %d", c.available, c.n, start, colW, c.start, c.colW)
		}
	}
}

func TestNavigation(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, ".hidden", "x"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644))

	m, err := newModel(Config{}, root)
	must(t, err)
	m.hasZoxide = false

	if names(m.active()) != "a,file.txt" {
		t.Fatalf("dirs first, hidden skipped: got %s", names(m.active()))
	}

	m.handleKey("l") // into a
	m.handleKey("l") // into a/b
	if len(m.cols) != 3 || m.active().path != filepath.Join(root, "a", "b") {
		t.Fatalf("expected 3 columns ending in a/b, got %d at %s", len(m.cols), m.active().path)
	}

	m.handleKey("h")
	m.handleKey("h")
	m.handleKey("h") // past the first column: parent becomes the only column
	if len(m.cols) != 1 || m.active().path != filepath.Dir(root) {
		t.Fatalf("expected parent of root, got %d cols at %s", len(m.cols), m.active().path)
	}
	if e, _ := m.active().selected(); e.name != filepath.Base(root) {
		t.Fatalf("cursor should be on the folder we came from, got %s", e.name)
	}

	m.resetTo(root, "")
	m.handleKey(".")
	m.handleKey("g")
	m.handleKey("g") // gg: top, which is .hidden now
	m.handleKey("l")
	m.handleKey(".") // hiding again must drop the .hidden column
	if len(m.cols) != 1 {
		t.Fatalf("hidden column should be dropped, got %d cols", len(m.cols))
	}
}

func names(c *column) string {
	s := ""
	for i, e := range c.entries {
		if i > 0 {
			s += ","
		}
		s += e.name
	}
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBookmarks(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfgFile := configPath()
	must(t, os.MkdirAll(filepath.Dir(cfgFile), 0o755))
	must(t, os.WriteFile(cfgFile, []byte("# my comment\nicons = false\nbookmarks = [\n  \"/tmp/Desktop\",\n]\n\n[keys]\nup = \"k\"\n"), 0o644))

	cfg, err := loadConfig()
	must(t, err)
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "Dotfiles"), 0o755))
	m, err := newModel(cfg, root)
	must(t, err)

	m.handleKey("m")                                             // cursor is on Dotfiles
	want := map[string]string{"Desktop": "gd", "Dotfiles": "go"} // d taken, o next letter
	for _, b := range m.bookmarks {
		if want[b.Name] != b.Key {
			t.Errorf("%s got key %q, want %q", b.Name, b.Key, want[b.Name])
		}
	}

	data, _ := os.ReadFile(cfgFile)
	s := string(data)
	if !strings.Contains(s, "# my comment") || !strings.Contains(s, "[keys]") || !strings.Contains(s, filepath.Join(root, "Dotfiles")) {
		t.Fatalf("config not updated in place:\n%s", s)
	}

	m.sidebarFocus, m.sidebarCursor = true, 0
	m.handleKey("d")
	cfg, err = loadConfig()
	must(t, err)
	if len(cfg.Bookmarks) != 1 || filepath.Base(cfg.Bookmarks[0]) != "Dotfiles" {
		t.Fatalf("remove failed: %v", cfg.Bookmarks)
	}
}

func TestFitImage(t *testing.T) {
	// 1000x500 image into 50x40 cells of 10x20px: width-bound, 500x250px = 50x13 cells
	cols, rows, pxW, pxH := fitImage(1000, 500, 50, 40, 10, 20)
	if cols != 50 || rows != 13 || pxW != 500 || pxH != 250 {
		t.Errorf("got %d %d %d %d", cols, rows, pxW, pxH)
	}
	// small icon is never upscaled
	if cols, rows, _, _ := fitImage(16, 16, 50, 40, 10, 20); cols != 2 || rows != 1 {
		t.Errorf("icon got %dx%d", cols, rows)
	}
}

func TestIsTextFile(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{"a.go": []byte("package a\n"), "empty": nil, "bin": {0, 1, 2}, "x.png": []byte("text")}
	want := map[string]bool{"a.go": true, "empty": true, "bin": false, "x.png": false}
	for name, data := range cases {
		p := filepath.Join(dir, name)
		must(t, os.WriteFile(p, data, 0o644))
		if got := isTextFile(p); got != want[name] {
			t.Errorf("isTextFile(%s) = %v", name, got)
		}
	}
}

func TestSelection(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		must(t, os.WriteFile(filepath.Join(root, n), nil, 0o644))
	}
	m, err := newModel(Config{}, root)
	must(t, err)

	m.handleKey(" ")
	m.handleKey(" ") // a, b; cursor moves down after each
	m.handleKey("j")
	m.handleKey("j") // on e
	m.handleKey("v")
	m.handleKey("j")
	m.handleKey("j") // e..g
	m.handleKey(" ")
	got := []string{}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		if m.selected[filepath.Join(root, n)] {
			got = append(got, n)
		}
	}
	if strings.Join(got, "") != "abefg" || m.visual != nil {
		t.Fatalf("selected %v, visual %v", got, m.visual)
	}

	m.handleKey("k")
	m.handleKey(" ") // unselect f
	if m.selected[filepath.Join(root, "f")] || len(m.selected) != 4 {
		t.Fatalf("toggle off failed: %v", m.selected)
	}

	m.handleKey("v")
	m.handleKey("esc") // cancels visual, keeps selection
	if m.visual != nil || len(m.selected) != 4 {
		t.Fatal("esc should only cancel visual")
	}
	m.handleKey("esc")
	if len(m.selected) != 0 {
		t.Fatal("second esc should clear selection")
	}
}

func TestVisualToggleAndInvert(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d"} {
		must(t, os.WriteFile(filepath.Join(root, n), nil, 0o644))
	}
	m, err := newModel(Config{}, root)
	must(t, err)
	sel := func() string {
		s := ""
		for _, n := range []string{"a", "b", "c", "d"} {
			if m.selected[filepath.Join(root, n)] {
				s += n
			}
		}
		return s
	}

	for _, k := range []string{"v", "j", "j", " "} { // select a..c
		m.handleKey(k)
	}
	for _, k := range []string{"gg", "v", "j", " "} { // a..b fully selected: unselect
		for _, r := range k {
			m.handleKey(string(r))
		}
	}
	if got := sel(); got != "c" {
		t.Fatalf("after unselecting a..b: %q", got)
	}
	for _, k := range []string{"v", "j", "j", " "} { // b..d partly selected (c): select all
		m.handleKey(k)
	}
	if got := sel(); got != "bcd" {
		t.Fatalf("partial range should select all: %q", got)
	}
	m.handleKey("ctrl+r")
	if got := sel(); got != "a" {
		t.Fatalf("invert: %q", got)
	}
}

func TestSetRoot(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755))
	m, err := newModel(Config{}, root)
	must(t, err)
	m.handleKey("l")
	m.handleKey("l")
	m.handleKey("H")
	if len(m.cols) != 1 || m.active().path != filepath.Join(root, "a", "b") {
		t.Fatalf("got %d cols at %s", len(m.cols), m.active().path)
	}
	if e, _ := m.active().selected(); e.name != "c" {
		t.Fatalf("cursor lost: %s", e.name)
	}
}

func TestFilterAndSort(t *testing.T) {
	root := t.TempDir()
	for i, n := range []string{"b.png", "a.txt", "C.png", "big.bin"} {
		must(t, os.WriteFile(filepath.Join(root, n), make([]byte, i*100), 0o644))
	}
	m, err := newModel(Config{}, root)
	must(t, err)
	press := func(ks ...string) {
		for _, k := range ks {
			runCmdMsg(m, m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}))
		}
	}

	press("/", "p", "n", "g")
	if names(m.active()) != "b.png,C.png" {
		t.Fatalf("filter: %s", names(m.active()))
	}
	m.update(tea.KeyMsg{Type: tea.KeyEnter})
	m.update(tea.KeyMsg{Type: tea.KeyCtrlR}) // invert only touches filtered entries
	if len(m.selected) != 2 {
		t.Fatalf("invert under filter selected %d", len(m.selected))
	}
	m.update(tea.KeyMsg{Type: tea.KeyEsc}) // clears filter first
	if m.active().filter != "" || len(m.selected) != 2 {
		t.Fatal("esc should clear filter before selection")
	}

	press(",", "s")
	if names(m.active()) != "big.bin,C.png,a.txt,b.png" {
		t.Fatalf("size sort: %s", names(m.active()))
	}
	press(",", "r")
	if names(m.active()) != "b.png,a.txt,C.png,big.bin" {
		t.Fatalf("reverse: %s", names(m.active()))
	}
}

func TestJumpHistory(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "other"), 0o755))
	m, err := newModel(Config{Bookmarks: []string{filepath.Join(root, "other")}}, root)
	must(t, err)
	m.hasZoxide = false
	m.handleKey("l")
	m.handleKey("l") // root / a / b
	m.handleKey("g")
	m.handleKey("o") // bookmark "other" → go
	if m.active().path != filepath.Join(root, "other") {
		t.Fatalf("bookmark jump: %s", m.active().path)
	}
	m.handleKey("[")
	if len(m.cols) != 3 || m.active().path != filepath.Join(root, "a", "b") {
		t.Fatalf("back should restore all columns, got %d at %s", len(m.cols), m.active().path)
	}
	m.handleKey("]")
	if m.active().path != filepath.Join(root, "other") {
		t.Fatalf("forward: %s", m.active().path)
	}
}

func TestGitStatus(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	must(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("a"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("k"), 0o644))
	run("init", "-q")
	run("add", ".")
	run("-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "init")
	must(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("changed"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "new.txt"), nil, 0o644))

	msg := loadGit(filepath.Join(root, "src"))().(gitMsg)
	if msg.root != root {
		t.Fatalf("root %q, want %q", msg.root, root)
	}
	want := map[string]byte{"src/a.go": 'M', "src": 'M', "new.txt": '?'}
	for rel, mark := range want {
		if got := msg.marks[filepath.Join(root, rel)]; got != mark {
			t.Errorf("%s: %q, want %q", rel, got, mark)
		}
	}
	if _, ok := msg.marks[filepath.Join(root, "keep.txt")]; ok {
		t.Error("clean file should have no mark")
	}
}

func TestTabs(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "b"), 0o755))
	m, err := newModel(Config{}, root)
	must(t, err)
	m.hasZoxide = false

	m.handleKey("l") // tab 1: root/a
	m.handleKey("t") // tab 2 starts at root/a
	m.handleKey("h")
	m.handleKey("j")
	m.handleKey("l") // tab 2: root/b
	if m.active().path != filepath.Join(root, "b") {
		t.Fatalf("tab 2 at %s", m.active().path)
	}
	m.handleKey("1")
	if len(m.cols) != 2 || m.active().path != filepath.Join(root, "a") {
		t.Fatalf("tab 1 should keep its columns, got %d at %s", len(m.cols), m.active().path)
	}
	m.handleKey("g")
	m.handleKey("t")
	if m.tabIdx != 1 || m.active().path != filepath.Join(root, "b") {
		t.Fatal("gt should go to tab 2")
	}
	m.handleKey("ctrl+w")
	if len(m.tabs) != 1 || m.active().path != filepath.Join(root, "a") {
		t.Fatal("closing tab 2 should land on tab 1")
	}
}

func TestBatchedKeysAfterPromptGoIntoPrompt(t *testing.T) {
	root := t.TempDir()
	m, err := newModel(Config{}, root)
	must(t, err)
	m.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("nmd.txt")})
	if m.prompt == nil || m.prompt.input.Value() != "md.txt" || m.confirm != nil {
		t.Fatalf("rest of the batch must be typed into the prompt, got prompt=%v confirm=%v", m.prompt, m.confirm)
	}
}

func TestFormatMeta(t *testing.T) {
	cases := map[[2]string]string{
		{"kMDItemDurationSeconds", "3725.4"}:               "1h 2m5s",
		{"kMDItemDurationSeconds", "65"}:                   "1m5s",
		{"kMDItemCodecs", "(\n    \"H.264\",\n    AAC\n)"}: "H.264, AAC",
		{"kMDItemExposureTimeSeconds", "0.004"}:            "1/250 s",
		{"kMDItemTotalBitRate", "320000"}:                  "320 kbps",
		{"kMDItemTitle", ""}:                               "",
	}
	for in, want := range cases {
		if got := formatMeta(in[0], in[1]); got != want {
			t.Errorf("formatMeta(%s, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestSymlinkEntries(t *testing.T) {
	root := t.TempDir()
	must(t, os.Mkdir(filepath.Join(root, "dir"), 0o755))
	must(t, os.Symlink("dir", filepath.Join(root, "ok")))
	must(t, os.Symlink("missing", filepath.Join(root, "dead")))

	col, err := loadColumn(root, listOpts{})
	must(t, err)
	got := map[string]entry{}
	for _, e := range col.all {
		got[e.name] = e
	}
	if e := got["ok"]; e.link != "dir" || !e.isDir || e.broken {
		t.Errorf("ok: %+v", e)
	}
	if e := got["dead"]; e.link != "missing" || !e.broken {
		t.Errorf("dead: %+v", e)
	}
	if e := got["dir"]; e.link != "" || e.broken {
		t.Errorf("dir: %+v", e)
	}
}

func TestGoto(t *testing.T) {
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "project", "src"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "Pictures"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "project", "src", "main.go"), nil, 0o644))

	m, err := newModel(Config{}, root)
	must(t, err)
	m.hasZoxide = false
	typeText := func(s string) {
		for _, r := range s {
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		}
	}

	m.openGoto()
	if got := strings.Join(m.gotoPanel.dirs, ","); got != "Pictures,project" {
		t.Fatalf("empty input lists subfolders: got %s", got)
	}
	typeText("pr")
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if v := m.gotoPanel.input.Value(); v != "project/" {
		t.Fatalf("tab completes: got %q", v)
	}
	typeText("s") // not a full name: enter goes to the highlighted match
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.gotoPanel != nil || m.active().path != filepath.Join(root, "project", "src") {
		t.Fatalf("enter should jump to project/src, at %s", m.active().path)
	}

	m.openGoto()
	typeText(filepath.Join(root, "project", "src", "main.go")) // absolute path to a file
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if e, _ := m.active().selected(); m.active().path != filepath.Join(root, "project", "src") || e.name != "main.go" {
		t.Fatalf("file path opens its folder with cursor on it, at %s on %s", m.active().path, e.name)
	}

	m.openGoto()
	typeText("nope")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.gotoPanel == nil || m.gotoPanel.err == nil {
		t.Fatal("missing path keeps the panel open with an error")
	}
}

func TestRightDoesNotOpenFiles(t *testing.T) {
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644))
	m, err := newModel(Config{}, root)
	must(t, err)
	m.hasZoxide = false
	if cmd := m.handleKey("l"); cmd != nil || len(m.cols) != 1 {
		t.Fatal("l on a file should do nothing")
	}
}
