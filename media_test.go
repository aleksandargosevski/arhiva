package main

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestParseSize(t *testing.T) {
	valid := map[string]sizeSpec{
		"1920":      {longest: 1920},
		"50%":       {percent: 50},
		"800x600":   {w: 800, h: 600},
		"800 X 600": {w: 800, h: 600},
	}
	for in, want := range valid {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-5", "abc", "100%", "150%", "0%", "800x", "x600", "800x0"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) should fail", in)
		}
	}
}

func TestParseTimestamp(t *testing.T) {
	valid := map[string]float64{"90": 90, "1:30": 90, "1:02:03": 3723, "1:30.5": 90.5, "0": 0, "0:05": 5}
	for in, want := range valid {
		if got, err := parseTimestamp(in); err != nil || got != want {
			t.Errorf("parseTimestamp(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "1:60", "1:2:3:4", "-5", "1.5:30"} {
		if _, err := parseTimestamp(in); err == nil {
			t.Errorf("parseTimestamp(%q) should fail", in)
		}
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		in         string
		duration   float64
		start, end float64
	}{
		{"0:10-1:30", 200, 10, 90},
		{"1:30-", 200, 90, 0},
		{"-0:45", 200, 0, 45},
		{"10-500", 200, 10, 0}, // past the end runs to the end
		{"10-20", 0, 10, 20},   // unknown duration
	}
	for _, c := range cases {
		start, end, err := parseRange(c.in, c.duration)
		if err != nil || start != c.start || end != c.end {
			t.Errorf("parseRange(%q) = %v %v %v; want %v %v", c.in, start, end, err, c.start, c.end)
		}
	}
	for _, in := range []string{"10", "20-10", "10-10", "300-", "-", "0-"} {
		if _, _, err := parseRange(in, 200); err == nil {
			t.Errorf("parseRange(%q) should fail", in)
		}
	}
}

func TestFormatStamp(t *testing.T) {
	cases := map[float64]string{0: "0.00", 90: "1.30", 3723: "1.02.03", 90.5: "1.30.5", 59.99: "1.00"}
	for in, want := range cases {
		if got := formatStamp(in, "."); got != want {
			t.Errorf("formatStamp(%v) = %q, want %q", in, got, want)
		}
	}
	if got := formatStamp(222, ":"); got != "3:42" {
		t.Errorf("formatStamp with colon = %q", got)
	}
}

func TestSuffixedNameAndConvertTargets(t *testing.T) {
	if got := suffixedName("/a/photo.jpg", "1920"); got != "photo (1920).jpg" {
		t.Errorf("suffixedName = %q", got)
	}
	if got := suffixedName("/a/photo.webp", "50%"); got != "photo (50%).png" {
		t.Errorf("webp should become png, got %q", got)
	}
	if got := convertTargets([]string{"a.JPEG", "b.jpg"}); slices.Contains(got, "jpg") {
		t.Errorf("jpg should not be offered for jpgs: %v", got)
	}
	if got := convertTargets([]string{"a.jpg", "b.png"}); !slices.Equal(got, convertFormats) {
		t.Errorf("mixed formats should offer everything: %v", got)
	}
}

func TestBatchSummary(t *testing.T) {
	paths := []string{"/a/1.jpg", "/a/2.jpg", "/a/3.txt"}
	if got := batchSummary("Resized", paths[:1], 1, nil); got != "Resized 1.jpg" {
		t.Errorf("got %q", got)
	}
	got := batchSummary("Resized", paths, 1, map[string]int{"already smaller": 1, "not an image": 1})
	if want := "Resized 1 of 3 (skipped: 1 already smaller, 1 not an image)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestImageOps(t *testing.T) {
	if _, err := exec.LookPath("sips"); err != nil {
		t.Skip("sips not available")
	}
	root := t.TempDir()
	writePNG(t, filepath.Join(root, "big.png"), 400, 200)
	writePNG(t, filepath.Join(root, "small.png"), 100, 50)
	must(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hi"), 0o644))
	m, err := newModel(Config{}, root)
	must(t, err)

	run := func(sel []string, key string, picks ...string) {
		t.Helper()
		clear(m.selected)
		for _, s := range sel {
			m.selected[filepath.Join(root, s)] = true
		}
		var cmd tea.Cmd
		for _, k := range key {
			cmd = m.handleKey(string(k))
		}
		for _, p := range picks {
			if value, ok := strings.CutPrefix(p, "="); ok { // typed into a prompt
				if m.prompt == nil {
					t.Fatalf("%s: no prompt for %q", key, value)
				}
				submit := m.prompt.submit
				m.prompt = nil
				cmd = submit(value)
				continue
			}
			if m.choice == nil {
				t.Fatalf("%s: no choice dialog for %q", key, p)
			}
			i := slices.Index(m.choice.options, p)
			if i < 0 {
				t.Fatalf("%s: %q not in %v", key, p, m.choice.options)
			}
			m.choice.cursor = i
			cmd = m.updateChoice(tea.KeyMsg{Type: tea.KeyEnter})
		}
		runCmdMsg(m, cmd)
	}

	run([]string{"big.png", "small.png", "notes.txt"}, "er", customSize, "=200")
	if m.statusIsErr {
		t.Fatal(m.status)
	}
	if w, h, err := imageSize(filepath.Join(root, "big (200).png")); err != nil || w != 200 || h != 100 {
		t.Fatalf("resize: %dx%d %v", w, h, err)
	}
	if exists(filepath.Join(root, "small (200).png")) {
		t.Fatal("small image should be skipped, not enlarged")
	}
	if want := "Resized 1 of 3 (skipped: 1 already smaller, 1 not an image)"; m.status != want {
		t.Fatalf("status %q, want %q", m.status, want)
	}
	if e, _ := m.active().selected(); e.name != "big (200).png" {
		t.Fatalf("cursor should be on the result, got %s", e.name)
	}

	run([]string{"big.png"}, "ec", "jpg")
	if f := normalExt(filepath.Join(root, "big.jpg")); !isSipsImage(filepath.Join(root, "big.jpg")) || f != "jpg" {
		t.Fatalf("convert failed: %s", m.status)
	}
	if m.lastPick["convert"] != "jpg" {
		t.Fatal("last pick not remembered")
	}

	run([]string{"big.png"}, "ef", "rotate right")
	if w, h, err := imageSize(filepath.Join(root, "big (rotated right).png")); err != nil || w != 200 || h != 400 {
		t.Fatalf("rotate: %dx%d %v", w, h, err)
	}

	run([]string{"big.jpg"}, "eo", "low (50)")
	if m.statusIsErr {
		t.Fatal(m.status)
	}

	before := len(m.undoStack)
	runCmdMsg(m, m.undo())
	if len(m.undoStack) != before-1 {
		t.Fatal("undo did not pop")
	}
}

func TestRemoveBackground(t *testing.T) {
	sample := "/Library/User Pictures/Animals/Parrot.heic"
	if !exists(sample) {
		t.Skip("no sample photo")
	}
	root := t.TempDir()
	src := filepath.Join(root, "parrot.heic")
	must(t, runCmd("cp", sample, src))
	dest, err := removeBackground(src)
	must(t, err)
	if filepath.Base(dest) != "parrot (no bg).png" {
		t.Fatalf("unexpected name %s", dest)
	}
	out, err := exec.Command("sips", "-g", "hasAlpha", dest).Output()
	must(t, err)
	if !strings.Contains(string(out), "hasAlpha: yes") {
		t.Fatalf("result has no alpha: %s", out)
	}

	blank := filepath.Join(root, "blank.png")
	writeSolidPNG(t, blank)
	if _, err := removeBackground(blank); err == nil || err.Error() != "no subject found" {
		t.Fatalf("blank image: want skip, got %v", err)
	}
}

func TestTrim(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "tone.m4a")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=duration=10", "-c:a", "aac", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	m, err := newModel(Config{}, root)
	must(t, err)
	m.active().selectName("tone.m4a")
	m.handleKey("e")
	m.handleKey("t")
	if m.prompt == nil {
		t.Fatalf("no trim prompt: %s", m.status)
	}
	p := m.prompt
	m.prompt = nil
	runCmdMsg(m, p.submit("0:02-0:05"))
	dest := filepath.Join(root, "tone (0.02-0.05).m4a")
	if d := mediaDuration(dest); d < 2.5 || d > 3.5 {
		t.Fatalf("trimmed duration %v, status %q", d, m.status)
	}
}

func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	encodePNG(t, path, img)
}

func writeSolidPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 200))
	for x := range 200 {
		for y := range 200 {
			img.Set(x, y, color.Gray{128})
		}
	}
	encodePNG(t, path, img)
}

func encodePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	f, err := os.Create(path)
	must(t, err)
	defer f.Close()
	must(t, png.Encode(f, img))
}
