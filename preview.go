package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"golang.org/x/sys/unix"
)

const (
	maxTextPreviewBytes = 64 * 1024
	maxPreviewLines     = 2000
	placeholderRune     = '\U0010EEEE'
)

var (
	decodableImages = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".tif": true, ".tiff": true}
	// Rendered to PNG by macOS QuickLook.
	// Videos QuickLook can thumbnail are included: they preview as a frame plus metadata.
	quickLookImages = map[string]bool{".svg": true, ".svgz": true, ".pdf": true, ".heic": true, ".heif": true, ".avif": true, ".psd": true, ".ico": true, ".icns": true, ".eps": true, ".ai": true, ".mp4": true, ".mov": true, ".m4v": true}
	// Media without a picture: previewed as metadata only.
	mediaOnly = map[string]bool{".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".flac": true, ".aiff": true, ".ogg": true, ".opus": true, ".mkv": true, ".avi": true, ".webm": true, ".wmv": true}
)

type previewKey struct {
	path string
	w, h int
	opts listOpts // folder previews depend on it
}

type preview struct {
	proto        string // "kitty" or "blocks"
	tmux         bool
	cellW, cellH int
	theme        string
	icons        bool
	gen          *atomic.Int64

	key     previewKey
	modTime time.Time // of key.path when requested, to notice edits
	stale   bool      // reload even though the key didn't change
	scroll  int
	label   string
	lines   []string
	imageID int // currently transmitted kitty image, 0 = none
}

type previewMsg struct {
	key        previewKey
	label      string
	lines      []string
	png        []byte // kitty mode: image to transmit, shown as cols×rows cells
	cols, rows int
	meta       []string // shown below the image
}

func newPreview(cfg Config) preview {
	p := preview{
		proto: detectImageProtocol(cfg.ImageProtocol),
		tmux:  os.Getenv("TMUX") != "",
		theme: cfg.SyntaxTheme,
		icons: cfg.Icons,
		gen:   &atomic.Int64{},
	}
	p.updateCellSize()
	return p
}

// syncPreview (re)loads the preview when the entry under the cursor or the preview size changed.
func (m *model) syncPreview() tea.Cmd {
	if m.finder != nil || m.help != nil || m.width == 0 {
		return nil
	}
	var key previewKey
	if path := m.previewPath(); path != "" {
		_, _, _, w := m.layout()
		key = previewKey{path, w - 1, m.bodyHeight() - 1, m.listOpts()}
	}
	return m.preview.request(key)
}

func (m *model) previewPath() string {
	col := m.active()
	if e, ok := col.selected(); ok {
		return filepath.Join(col.path, e.name)
	}
	return ""
}

// refresh forces the next syncPreview to reload, e.g. after the file was edited.
func (p *preview) refresh() { p.stale = true }

func (p *preview) refreshIfChanged() bool {
	if p.key.path == "" || p.stale {
		return false
	}
	if info, err := os.Stat(p.key.path); err != nil || !info.ModTime().Equal(p.modTime) {
		p.refresh()
		return true
	}
	return false
}

func (p *preview) request(key previewKey) tea.Cmd {
	if key == p.key && !p.stale {
		return nil
	}
	if key.path != p.key.path { // same file reloading keeps showing the old content meanwhile
		p.label, p.lines, p.scroll = "", nil, 0
	}
	p.key, p.stale = key, false
	p.modTime = time.Time{}
	if info, err := os.Stat(key.path); err == nil {
		p.modTime = info.ModTime()
	}
	gen := p.gen.Add(1)
	if key.path == "" || key.w < 1 || key.h < 1 {
		return nil
	}
	env := *p // loader gets a snapshot; the UI keeps mutating p
	return func() tea.Msg {
		msg := env.load(key, func() bool { return env.gen.Load() != gen })
		if msg == nil {
			return nil
		}
		return *msg
	}
}

func (p *preview) receive(msg previewMsg) {
	if msg.key != p.key {
		return
	}
	p.label, p.lines = msg.label, msg.lines
	if msg.png == nil {
		return
	}
	old := p.imageID
	p.imageID = p.imageID%240 + 16 // 16..255: sent as a 256-color fg, which survives tmux untouched
	var out strings.Builder
	if old != 0 {
		out.WriteString(p.wrap(fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", old)))
	}
	out.WriteString(p.transmit(msg.png, msg.cols, msg.rows))
	// Written straight to the terminal: os.Stdout serializes writes, so this can't interleave with a frame.
	if _, err := os.Stdout.WriteString(out.String()); err != nil {
		p.lines = []string{errStyle.Render("image: " + err.Error())}
		return
	}
	p.lines = append(placeholderLines(p.imageID, msg.cols, msg.rows), msg.meta...)
}

// cleanup deletes the transmitted image from terminal memory; call after the program exits.
func (p *preview) cleanup() {
	if p.imageID != 0 {
		os.Stdout.WriteString(p.wrap(fmt.Sprintf("\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", p.imageID)))
	}
}

func (p *preview) scrollBy(delta, h int) {
	p.scroll = clamp(p.scroll+delta, 0, max(len(p.lines)-(h-1), 0))
}

func (p *preview) view(w, h int) string {
	p.scrollBy(0, h) // content or height may have changed
	label := p.label
	if p.scroll > 0 {
		label += fmt.Sprintf(" · line %d", p.scroll+1)
	}
	lines := []string{headerStyle.Render(fit(" "+label, w))}
	for _, l := range p.lines[p.scroll:] {
		if len(lines) >= h {
			break
		}
		lines = append(lines, " "+l+strings.Repeat(" ", max(w-1-lipgloss.Width(l), 0)))
	}
	for len(lines) < h {
		lines = append(lines, strings.Repeat(" ", w))
	}
	return strings.Join(lines, "\n")
}

// ---- loading (runs off the UI goroutine) ----

func (p preview) load(key previewKey, stale func() bool) *previewMsg {
	proto, cellW, cellH := p.proto, p.cellW, p.cellH
	msg := &previewMsg{key: key}
	info, err := os.Stat(key.path)
	if err != nil {
		msg.lines = []string{errStyle.Render(err.Error())}
		return msg
	}
	if info.IsDir() {
		msg.label, msg.lines = dirPreview(key, p.icons)
		return msg
	}
	msg.label = humanSize(info.Size())
	ext := strings.ToLower(filepath.Ext(key.path))
	isArchiveFile := archiveExt(key.path) != "" && compressors[archiveExt(key.path)] == nil
	if !isImage(key.path) && !mediaOnly[ext] && !isArchiveFile {
		msg.lines = textPreview(key, p.theme)
		return msg
	}

	// Images, media and archives are expensive: skip the ones scrolled past quickly.
	time.Sleep(40 * time.Millisecond)
	if stale() {
		return nil
	}
	if isArchiveFile {
		var count int
		msg.lines, count = archivePreview(key)
		msg.label += fmt.Sprintf(" · %d entries", count)
		return msg
	}
	meta := mediaMeta(key.path, key.w-1)
	if mediaOnly[ext] {
		msg.lines = meta
		if len(meta) == 0 {
			msg.lines = []string{dimStyle.Render("no metadata (is Spotlight indexing this folder?)")}
		}
		return msg
	}
	if len(meta) > 0 {
		meta = append([]string{""}, meta...)
	}
	key.h = max(key.h-len(meta), 3) // the image fits in what the metadata leaves
	var img image.Image
	if quickLookImages[ext] {
		img, err = quickLook(key.path, max(key.w*cellW, key.h*cellH))
	} else {
		img, err = decodeImage(key.path)
	}
	if err != nil {
		msg.lines = append([]string{errStyle.Render("cannot preview: " + err.Error())}, meta...)
		return msg
	}
	b := img.Bounds()
	if decodableImages[ext] {
		msg.label += fmt.Sprintf(" · %d×%d", b.Dx(), b.Dy())
	}

	if proto == "kitty" {
		cols, rows, pxW, pxH := fitImage(b.Dx(), b.Dy(), key.w, key.h, cellW, cellH)
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, scale(img, pxW, pxH, false)); err != nil {
			msg.lines = []string{errStyle.Render("cannot encode image: " + err.Error())}
			return msg
		}
		msg.png, msg.cols, msg.rows, msg.meta = buf.Bytes(), cols, rows, meta
		return msg
	}
	cols, rows, _, _ := fitImage(b.Dx(), b.Dy(), key.w, key.h, cellW, cellH)
	msg.lines = append(halfBlocks(scale(img, cols, rows*2, true)), meta...)
	return msg
}

// archivePreview lists an archive's contents with bsdtar, without extracting.
func archivePreview(key previewKey) ([]string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tar", "-tf", key.path).Output()
	if err != nil && len(out) == 0 {
		return []string{errStyle.Render("cannot list archive: " + err.Error())}, 0
	}
	names := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	lines := make([]string, 0, min(len(names), maxPreviewLines))
	for _, n := range names[:min(len(names), maxPreviewLines)] {
		style := fileStyle
		if strings.HasSuffix(n, "/") {
			style = dirStyle
		}
		lines = append(lines, style.Render(fit(sanitize(n), key.w-1)))
	}
	return lines, len(names)
}

var mdlsAttrs = []struct{ key, label string }{
	{"kMDItemTitle", "Title"},
	{"kMDItemAuthors", "Artist"},
	{"kMDItemAlbum", "Album"},
	{"kMDItemDurationSeconds", "Duration"},
	{"kMDItemPixelWidth", "Width"},
	{"kMDItemPixelHeight", "Height"},
	{"kMDItemCodecs", "Codecs"},
	{"kMDItemTotalBitRate", "Bitrate"},
	{"kMDItemAudioSampleRate", "Sample rate"},
	{"kMDItemNumberOfPages", "Pages"},
	{"kMDItemAcquisitionMake", "Camera"},
	{"kMDItemAcquisitionModel", "Model"},
	{"kMDItemLensModel", "Lens"},
	{"kMDItemFocalLength", "Focal length"},
	{"kMDItemFNumber", "Aperture"},
	{"kMDItemExposureTimeSeconds", "Exposure"},
	{"kMDItemISOSpeed", "ISO"},
	{"kMDItemContentCreationDate", "Created"},
	{"kMDItemLatitude", "Latitude"},
	{"kMDItemLongitude", "Longitude"},
	{"kMDItemWhereFroms", "From"},
}

// mediaMeta reads Spotlight metadata with mdls; files outside indexed folders have none.
func mediaMeta(path string, w int) []string {
	// mdls -raw prints values in alphabetical order of attribute name, not argument order.
	keys := make([]string, len(mdlsAttrs))
	for i, a := range mdlsAttrs {
		keys[i] = a.key
	}
	slices.Sort(keys)
	args := []string{"-raw", "-nullMarker", ""}
	for _, k := range keys {
		args = append(args, "-name", k)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mdls", append(args, path)...).Output()
	if err != nil {
		return nil
	}
	raw := strings.Split(string(out), "\x00")
	if len(raw) < len(keys) {
		return nil
	}
	values := map[string]string{}
	for i, k := range keys {
		values[k] = raw[i]
	}
	var lines []string
	for _, a := range mdlsAttrs {
		v := formatMeta(a.key, values[a.key])
		if v == "" {
			continue
		}
		lines = append(lines, dimStyle.Render(fmt.Sprintf("%-13s", a.label))+fileStyle.Render(runewidth.Truncate(sanitize(v), max(w-13, 1), "…")))
	}
	return lines
}

func formatMeta(key, v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "(") { // arrays come as "(\n    a,\n    \"b c\"\n)"
		var items []string
		for _, it := range strings.Split(strings.Trim(v, "()"), ",") {
			if it = strings.Trim(strings.TrimSpace(it), `"`); it != "" {
				items = append(items, it)
			}
		}
		v = strings.Join(items, ", ")
	}
	if v == "" {
		return ""
	}
	f, numErr := strconv.ParseFloat(v, 64)
	switch key {
	case "kMDItemDurationSeconds":
		if numErr == nil {
			d := time.Duration(f * float64(time.Second)).Round(time.Second)
			return strings.TrimSuffix(strings.Replace(d.String(), "h", "h ", 1), "0s")
		}
	case "kMDItemTotalBitRate":
		if numErr == nil {
			return fmt.Sprintf("%.0f kbps", f/1000)
		}
	case "kMDItemAudioSampleRate":
		if numErr == nil {
			return fmt.Sprintf("%.1f kHz", f/1000)
		}
	case "kMDItemFNumber":
		return "f/" + v
	case "kMDItemFocalLength":
		return v + " mm"
	case "kMDItemExposureTimeSeconds":
		if numErr == nil && f > 0 && f < 1 {
			return fmt.Sprintf("1/%.0f s", 1/f)
		}
		return v + " s"
	case "kMDItemContentCreationDate":
		return strings.TrimSuffix(v, " +0000")
	}
	return v
}

func dirPreview(key previewKey, icons bool) (string, []string) {
	col, err := loadColumn(key.path, key.opts)
	if err != nil {
		return "", []string{errStyle.Render(err.Error())}
	}
	label := fmt.Sprintf("%d items", len(col.entries))
	if len(col.entries) == 0 {
		return label, []string{dimStyle.Render("empty")}
	}
	lines := make([]string, 0, min(len(col.entries), maxPreviewLines))
	for _, e := range col.entries[:min(len(col.entries), maxPreviewLines)] {
		text := e.name
		if icons {
			text = entryIcon(e) + " " + e.name
		} else if e.isDir {
			text += "/"
		}
		style := fileStyle
		if e.isDir {
			style = dirStyle
		}
		lines = append(lines, style.Render(fit(text, key.w-1)))
	}
	return label, lines
}

func isImage(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return decodableImages[ext] || quickLookImages[ext]
}

// isTextFile reports whether path should be opened in $EDITOR rather than the default app.
func isTextFile(path string) bool {
	if isImage(path) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8*1024))
	return err == nil && !isBinary(data)
}

func textPreview(key previewKey, theme string) []string {
	f, err := os.Open(key.path)
	if err != nil {
		return []string{errStyle.Render(err.Error())}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxTextPreviewBytes))
	if err != nil {
		return []string{errStyle.Render(err.Error())}
	}
	if len(data) == 0 {
		return []string{dimStyle.Render("empty file")}
	}
	if isBinary(data) {
		return []string{dimStyle.Render("binary file")}
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.SplitN(text, "\n", maxPreviewLines+1)
	text = sanitize(strings.Join(lines[:min(len(lines), maxPreviewLines)], "\n"))

	out := highlight(key.path, text, theme)
	for i, l := range out {
		out[i] = ansi.Truncate(l, key.w-1, "")
	}
	return out
}

func highlight(path, text, theme string) []string {
	lexer := lexers.Match(filepath.Base(path))
	if lexer == nil {
		lexer = lexers.Analyse(text)
	}
	if lexer == nil {
		return strings.Split(text, "\n")
	}
	tokens, err := chroma.Coalesce(lexer).Tokenise(nil, text)
	if err != nil {
		return strings.Split(text, "\n")
	}
	style := styles.Get(theme)
	var b strings.Builder
	for _, tok := range tokens.Tokens() {
		colour := style.Get(tok.Type).Colour
		if !colour.IsSet() {
			b.WriteString(tok.Value)
			continue
		}
		st := lipgloss.NewStyle().Foreground(lipgloss.Color(colour.String()))
		// Style each line separately so colors never span a line break.
		for i, part := range strings.Split(tok.Value, "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			if part != "" {
				b.WriteString(st.Render(part))
			}
		}
	}
	return strings.Split(b.String(), "\n")
}

func isBinary(data []byte) bool {
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	// Drop a rune possibly cut in half by the read limit.
	for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
		data = data[:len(data)-1]
	}
	return !utf8.Valid(data)
}

// sanitize expands tabs and removes control characters, so file contents can't inject terminal escapes.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return '?'
		}
		return r
	}, strings.ReplaceAll(s, "\t", "    "))
}

func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func quickLook(path string, px int) (image.Image, error) {
	dir, err := os.MkdirTemp("", "arhiva-ql")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out, err := exec.Command("qlmanage", "-t", "-s", strconv.Itoa(max(px, 256)), "-o", dir, path).CombinedOutput()
	pngs, _ := filepath.Glob(filepath.Join(dir, "*.png"))
	if len(pngs) == 0 {
		if err == nil {
			err = errors.New("QuickLook produced no image")
		}
		return nil, fmt.Errorf("%w %s", err, strings.TrimSpace(string(out)))
	}
	img, err := decodeImage(pngs[0])
	if err != nil {
		return nil, err
	}
	return trimPadding(img), nil
}

// trimPadding crops the solid padding QuickLook adds around thumbnails (it renders into a square
// canvas); the padding color is taken from the bottom-right pixel.
func trimPadding(img image.Image) image.Image {
	b := img.Bounds()
	bg := img.At(b.Max.X-1, b.Max.Y-1)
	same := func(x, y int) bool { return img.At(x, y) == bg }
	rowIsBg := func(y int) bool {
		for x := b.Min.X; x < b.Max.X; x++ {
			if !same(x, y) {
				return false
			}
		}
		return true
	}
	colIsBg := func(x, maxY int) bool {
		for y := b.Min.Y; y < maxY; y++ {
			if !same(x, y) {
				return false
			}
		}
		return true
	}
	maxY := b.Max.Y
	for maxY > b.Min.Y+1 && rowIsBg(maxY-1) {
		maxY--
	}
	maxX := b.Max.X
	for maxX > b.Min.X+1 && colIsBg(maxX-1, maxY) {
		maxX--
	}
	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return img
	}
	return sub.SubImage(image.Rect(b.Min.X, b.Min.Y, maxX, maxY))
}

// fitImage fits an image into maxCols×maxRows cells keeping its aspect ratio, never upscaling.
// Returns the size in cells and in pixels.
func fitImage(imgW, imgH, maxCols, maxRows, cellW, cellH int) (cols, rows, pxW, pxH int) {
	s := math.Min(1, math.Min(float64(maxCols*cellW)/float64(imgW), float64(maxRows*cellH)/float64(imgH)))
	pxW, pxH = max(int(float64(imgW)*s), 1), max(int(float64(imgH)*s), 1)
	cols = clamp(int(math.Ceil(float64(pxW)/float64(cellW))), 1, min(maxCols, len(diacritics)))
	rows = clamp(int(math.Ceil(float64(pxH)/float64(cellH))), 1, min(maxRows, len(diacritics)))
	return cols, rows, pxW, pxH
}

func scale(img image.Image, w, h int, opaque bool) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	op := draw.Src
	if opaque { // flatten transparency onto black for half-blocks
		draw.Draw(dst, dst.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
		op = draw.Over
	}
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), op, nil)
	return dst
}

// halfBlocks draws two pixels per cell with "▀": foreground is the top pixel, background the bottom.
func halfBlocks(img image.Image) []string {
	b := img.Bounds()
	lines := make([]string, 0, b.Dy()/2)
	for y := b.Min.Y; y+1 < b.Max.Y; y += 2 {
		var s strings.Builder
		for x := b.Min.X; x < b.Max.X; x++ {
			tr, tg, tb, _ := img.At(x, y).RGBA()
			br, bg, bb, _ := img.At(x, y+1).RGBA()
			fmt.Fprintf(&s, "\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm▀", tr>>8, tg>>8, tb>>8, br>>8, bg>>8, bb>>8)
		}
		s.WriteString("\x1b[0m")
		lines = append(lines, s.String())
	}
	return lines
}

// ---- kitty graphics protocol ----

// transmit sends a PNG as a virtual placement (U=1) shown wherever placeholder cells with its id are drawn.
func (p *preview) transmit(pngData []byte, cols, rows int) string {
	const chunk = 4096
	data := base64.StdEncoding.EncodeToString(pngData)
	var out strings.Builder
	for i := 0; i < len(data); i += chunk {
		end := min(i+chunk, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		ctrl := fmt.Sprintf("m=%d", more)
		if i == 0 {
			ctrl = fmt.Sprintf("a=T,U=1,f=100,q=2,i=%d,c=%d,r=%d,m=%d", p.imageID, cols, rows, more)
		}
		out.WriteString(p.wrap("\x1b_G" + ctrl + ";" + data[i:end] + "\x1b\\"))
	}
	return out.String()
}

func (p *preview) wrap(seq string) string {
	if !p.tmux {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

func placeholderLines(id, cols, rows int) []string {
	lines := make([]string, rows)
	for r := range rows {
		var s strings.Builder
		fmt.Fprintf(&s, "\x1b[38;5;%dm", id)
		for c := range cols {
			s.WriteRune(placeholderRune)
			s.WriteRune(diacritics[r])
			s.WriteRune(diacritics[c])
		}
		s.WriteString("\x1b[39m")
		lines[r] = s.String()
	}
	return lines
}

func (p *preview) updateCellSize() {
	p.cellW, p.cellH = 10, 20
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err == nil && ws.Xpixel > 0 && ws.Ypixel > 0 && ws.Col > 0 && ws.Row > 0 {
		p.cellW, p.cellH = int(ws.Xpixel/ws.Col), int(ws.Ypixel/ws.Row)
	}
}

func detectImageProtocol(setting string) string {
	switch setting {
	case "kitty", "blocks":
		return setting
	}
	if os.Getenv("NVIM") != "" { // nvim's :terminal doesn't pass graphics through
		return "blocks"
	}
	if os.Getenv("KITTY_WINDOW_ID") != "" || os.Getenv("TERM") == "xterm-kitty" ||
		os.Getenv("TERM_PROGRAM") == "ghostty" || os.Getenv("GHOSTTY_RESOURCES_DIR") != "" {
		return "kitty"
	}
	return "blocks"
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
