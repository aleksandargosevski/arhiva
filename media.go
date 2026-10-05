package main

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

//go:embed removebg.js
var removeBackgroundScript string

const (
	customSize    = "custom…"
	encodeQuality = "85" // jpg / heic quality for convert
	maxWorkers    = 8
)

var (
	// Formats sips reads. It writes all of them except webp, so webp results are saved as png.
	sipsImages     = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".heic": true, ".heif": true, ".tif": true, ".tiff": true, ".gif": true, ".bmp": true, ".webp": true, ".avif": true}
	convertFormats = []string{"jpg", "png", "heic", "avif", "tiff", "gif", "bmp", "pdf"}
	resizePresets  = []string{"3840", "2560", "1920", "1280", "800", "50%", "25%", customSize}
	qualities      = []struct{ label, value string }{{"high (85)", "85"}, {"medium (70)", "70"}, {"low (50)", "50"}}
	transforms     = []struct {
		label, suffix string
		args          []string
	}{
		{"rotate right", "rotated right", []string{"-r", "90"}},
		{"rotate left", "rotated left", []string{"-r", "270"}},
		{"rotate 180°", "rotated 180", []string{"-r", "180"}},
		{"flip horizontal", "flipped horizontally", []string{"-f", "horizontal"}},
		{"flip vertical", "flipped vertically", []string{"-f", "vertical"}},
	}
	mediaFiles = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true, ".avi": true, ".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".flac": true, ".ogg": true, ".opus": true, ".aiff": true, ".aif": true}
)

// mediaAction starts an image or media operation on the targets. Results are always new files next
// to the originals; a whole batch is one undo step.
func (m *model) mediaAction(action string) tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	if action == "trim" {
		return m.startTrim(paths)
	}
	images := slices.DeleteFunc(slices.Clone(paths), func(p string) bool { return !isSipsImage(p) })
	if len(images) == 0 {
		m.setErr(fmt.Errorf("%s: not an image", countLabel(paths)))
		return nil
	}
	label := countLabel(images)

	switch action {
	case "convert":
		m.openChoice("convert", "Convert "+label+" to", convertTargets(images), func(format string) tea.Cmd {
			return m.runImageBatch(paths, "Converting", "Converted", func(src string) (string, error) {
				return convertImage(src, format)
			}, nil)
		})
	case "resize":
		m.openChoice("resize", "Resize "+label+" (longest side)", resizePresets, func(picked string) tea.Cmd {
			if picked == customSize {
				return m.openPrompt("Resize "+label+": 1500 (longest side), 30% or 800x600", "", 0, func(s string) tea.Cmd {
					return m.resize(paths, s)
				})
			}
			return m.resize(paths, picked)
		})
	case "optimize":
		return m.startOptimize(paths, images)
	case "rotate_flip":
		labels := make([]string, len(transforms))
		for i, t := range transforms {
			labels[i] = t.label
		}
		m.openChoice("rotate_flip", "Rotate / flip "+label, labels, func(picked string) tea.Cmd {
			t := transforms[slices.Index(labels, picked)]
			return m.runImageBatch(paths, "Transforming", "Transformed", func(src string) (string, error) {
				return produce(src, suffixedName(src, t.suffix), func(tmp string) error {
					return sipsKeepFormat(src, tmp, t.args...)
				})
			}, nil)
		})
	case "remove_background":
		return m.runImageBatch(paths, "Removing background of", "Removed background of", removeBackground, nil)
	}
	return nil
}

// ---- batch ----

// skipError marks a file that was deliberately left alone (e.g. already smaller); it's counted, not reported as an error.
type skipError string

func (s skipError) Error() string { return string(s) }

// runImageBatch runs fn on every path in parallel and reports a summary like
// "Resized 10 of 12 (2 skipped: already smaller)". extra, if set, is appended to the summary.
func (m *model) runImageBatch(paths []string, verb, past string, fn func(src string) (string, error), extra func() string) tea.Cmd {
	m.clearSelection()
	activeDir := m.active().path
	j := &job{label: verb + " " + countLabel(paths), items: true}
	j.total.Store(int64(len(paths)))
	m.jobs = append(m.jobs, j)

	return func() tea.Msg {
		created := make([]string, len(paths))
		errs := make([]error, len(paths))
		parallel(len(paths), func(i int) {
			created[i], errs[i] = fn(paths[i])
			j.done.Add(1)
		})

		done := opDoneMsg{undo: &undoStep{desc: strings.ToLower(past)}, job: j}
		skips := map[string]int{}
		var failures []error
		for i, err := range errs {
			var skip skipError
			switch {
			case errors.As(err, &skip):
				skips[string(skip)]++
			case err != nil:
				failures = append(failures, fmt.Errorf("%s: %w", filepath.Base(paths[i]), err))
			default:
				done.undo.created = append(done.undo.created, created[i])
				if filepath.Dir(created[i]) == activeDir {
					done.selectName = filepath.Base(created[i])
				}
			}
		}
		done.err = errors.Join(failures...)
		done.status = batchSummary(past, paths, len(done.undo.created), skips)
		if extra != nil && len(done.undo.created) > 0 {
			done.status += ", " + extra()
		}
		return done
	}
}

func batchSummary(past string, paths []string, succeeded int, skips map[string]int) string {
	if len(skips) == 0 && succeeded == len(paths) {
		return past + " " + countLabel(paths)
	}
	var reasons []string
	skipped := 0
	for _, reason := range slices.Sorted(maps.Keys(skips)) {
		reasons = append(reasons, fmt.Sprintf("%d %s", skips[reason], reason))
		skipped += skips[reason]
	}
	status := fmt.Sprintf("%s %d of %d", past, succeeded, len(paths))
	if skipped > 0 {
		status += " (skipped: " + strings.Join(reasons, ", ") + ")"
	}
	return status
}

// parallel calls fn(0..n-1) on up to maxWorkers goroutines.
func parallel(n int, fn func(i int)) {
	next := atomic.Int64{}
	var wg sync.WaitGroup
	for range min(runtime.NumCPU(), maxWorkers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				fn(i)
			}
		}()
	}
	wg.Wait()
}

var finalizeMu sync.Mutex

// produce writes a new file next to src: write fills a hidden temp file (same extension as name), which is
// then renamed to name, or "name (1).ext" if taken. Returns the created path.
func produce(src, name string, write func(tmp string) error) (string, error) {
	dir := filepath.Dir(src)
	f, err := os.CreateTemp(dir, ".arhiva-*"+filepath.Ext(name))
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	f.Close()
	if err := write(tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if info, err := os.Stat(tmp); err != nil || info.Size() == 0 {
		os.Remove(tmp)
		return "", errors.New("no output was written")
	}
	if err := os.Chmod(tmp, 0o644); err != nil { // CreateTemp makes 0600
		os.Remove(tmp)
		return "", err
	}

	// Parallel workers can target the same name ("a.heic" and "a.png" both to "a.jpg").
	finalizeMu.Lock()
	defer finalizeMu.Unlock()
	dest := uniquePath(dir, name, nil)
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// suffixedName turns "photo.jpg" + "1920" into "photo (1920).jpg"; webp becomes png since sips can't write it.
func suffixedName(src, suffix string) string {
	stem, ext := splitExt(filepath.Base(src))
	if strings.EqualFold(ext, ".webp") {
		ext = ".png"
	}
	return fmt.Sprintf("%s (%s)%s", stem, suffix, ext)
}

func isSipsImage(path string) bool {
	return sipsImages[strings.ToLower(filepath.Ext(path))] && isRegular(path)
}

// normalExt maps spelling variants to one format name: ".JPEG" → "jpg", ".tif" → "tiff".
func normalExt(path string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	switch ext {
	case "jpeg":
		return "jpg"
	case "tif":
		return "tiff"
	case "heif":
		return "heic"
	}
	return ext
}

// ---- sips ----

func sips(src, out string, args ...string) error {
	return runCmd("sips", append(args, src, "--out", out)...)
}

// sipsKeepFormat runs sips and keeps the source format, except webp which sips can only read.
func sipsKeepFormat(src, out string, args ...string) error {
	if normalExt(src) == "webp" {
		args = append(args, "-s", "format", "png")
	}
	return sips(src, out, args...)
}

// imageSize reads the pixel dimensions with sips.
func imageSize(path string) (w, h int, err error) {
	out, err := exec.Command("sips", "-g", "pixelWidth", "-g", "pixelHeight", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("sips: can't read the image size: %w", err)
	}
	for line := range strings.Lines(string(out)) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(value)
		switch key {
		case "pixelWidth":
			w = n
		case "pixelHeight":
			h = n
		}
	}
	if w == 0 || h == 0 {
		return 0, 0, errors.New("sips: can't read the image size")
	}
	return w, h, nil
}

// ---- convert ----

// convertTargets lists the formats to offer, without the one all images already have.
func convertTargets(images []string) []string {
	first := normalExt(images[0])
	if !all(images, func(p string) bool { return normalExt(p) == first }) {
		return convertFormats
	}
	return slices.DeleteFunc(slices.Clone(convertFormats), func(f string) bool { return f == first })
}

func convertImage(src, format string) (string, error) {
	if !isSipsImage(src) {
		return "", skipError("not an image")
	}
	if normalExt(src) == format {
		return "", skipError("already " + format)
	}
	stem, _ := splitExt(filepath.Base(src))
	return produce(src, stem+"."+format, func(tmp string) error {
		sipsFormat := map[string]string{"jpg": "jpeg"}[format]
		args := []string{"-s", "format", cmp.Or(sipsFormat, format)}
		if format == "jpg" || format == "heic" {
			args = append(args, "-s", "formatOptions", encodeQuality)
		}
		return sips(src, tmp, args...)
	})
}

// ---- resize ----

// sizeSpec is one of: longest side in px, a percentage (< 100), or an exact WxH.
type sizeSpec struct {
	longest, percent, w, h int
}

func parseSize(s string) (sizeSpec, error) {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	invalid := fmt.Errorf("invalid size %q, use 1500, 30%% or 800x600", s)
	positive := func(v string) (int, bool) {
		n, err := strconv.Atoi(v)
		return n, err == nil && n > 0
	}
	if p, ok := strings.CutSuffix(s, "%"); ok {
		n, valid := positive(p)
		if !valid {
			return sizeSpec{}, invalid
		}
		if n >= 100 {
			return sizeSpec{}, fmt.Errorf("%d%% would not make the image smaller", n)
		}
		return sizeSpec{percent: n}, nil
	}
	if ws, hs, ok := strings.Cut(s, "x"); ok {
		w, wok := positive(ws)
		h, hok := positive(hs)
		if !wok || !hok {
			return sizeSpec{}, invalid
		}
		return sizeSpec{w: w, h: h}, nil
	}
	n, ok := positive(s)
	if !ok {
		return sizeSpec{}, invalid
	}
	return sizeSpec{longest: n}, nil
}

func (s sizeSpec) String() string {
	switch {
	case s.percent > 0:
		return fmt.Sprintf("%d%%", s.percent)
	case s.longest > 0:
		return strconv.Itoa(s.longest)
	}
	return fmt.Sprintf("%dx%d", s.w, s.h)
}

func (m *model) resize(paths []string, input string) tea.Cmd {
	if input == "" {
		return nil
	}
	spec, err := parseSize(input)
	if err != nil {
		m.setErr(err)
		return nil
	}
	return m.runImageBatch(paths, "Resizing", "Resized", func(src string) (string, error) {
		return resizeImage(src, spec)
	}, nil)
}

// resizeImage never enlarges: images already within the longest side are skipped. An exact WxH is done as asked.
func resizeImage(src string, spec sizeSpec) (string, error) {
	if !isSipsImage(src) {
		return "", skipError("not an image")
	}
	w, h, err := imageSize(src)
	if err != nil {
		return "", err
	}
	var args []string
	switch {
	case spec.longest > 0:
		if max(w, h) <= spec.longest {
			return "", skipError("already smaller")
		}
		args = []string{"-Z", strconv.Itoa(spec.longest)}
	case spec.percent > 0:
		scaled := func(v int) string { return strconv.Itoa(max(int(math.Round(float64(v*spec.percent)/100)), 1)) }
		args = []string{"-z", scaled(h), scaled(w)}
	default:
		args = []string{"-z", strconv.Itoa(spec.h), strconv.Itoa(spec.w)}
	}
	return produce(src, suffixedName(src, spec.String()), func(tmp string) error {
		return sipsKeepFormat(src, tmp, args...)
	})
}

// ---- optimize ----

func (m *model) startOptimize(paths, images []string) tea.Cmd {
	hasPNG := slices.ContainsFunc(images, func(p string) bool { return normalExt(p) == "png" })
	hasLossy := slices.ContainsFunc(images, isLossyImage)
	pngTool := pngOptimizer()
	if !hasLossy && (!hasPNG || pngTool == "") {
		if hasPNG {
			m.setErr(errors.New("PNG optimize needs pngquant: brew install pngquant"))
		} else {
			m.setErr(errors.New("optimize works on jpg, heic and png"))
		}
		return nil
	}

	run := func(quality string) tea.Cmd {
		var before, after atomic.Int64
		return m.runImageBatch(paths, "Optimizing", "Optimized", func(src string) (string, error) {
			dest, err := optimizeImage(src, quality, pngTool)
			if err == nil {
				before.Add(fileSize(src))
				after.Add(fileSize(dest))
			}
			return dest, err
		}, func() string {
			b, a := before.Load(), after.Load()
			return fmt.Sprintf("%s → %s (−%d%%)", humanSize(b), humanSize(a), (b-a)*100/max(b, 1))
		})
	}
	if !hasLossy {
		return run("")
	}
	labels := make([]string, len(qualities))
	for i, q := range qualities {
		labels[i] = q.label
	}
	m.openChoice("optimize", "Optimize "+countLabel(images)+" (jpg / heic quality)", labels, func(picked string) tea.Cmd {
		return run(qualities[slices.Index(labels, picked)].value)
	})
	return nil
}

// optimizeImage re-encodes jpg/heic at quality and shrinks png with pngTool. A result that isn't smaller is discarded.
func optimizeImage(src, quality, pngTool string) (string, error) {
	var write func(tmp string) error
	switch {
	case isLossyImage(src) && isSipsImage(src):
		write = func(tmp string) error { return sips(src, tmp, "-s", "formatOptions", quality) }
	case normalExt(src) == "png" && isRegular(src):
		if pngTool == "" {
			return "", skipError("png without pngquant")
		}
		write = func(tmp string) error { return optimizePNG(pngTool, src, tmp) }
	default:
		return "", skipError("not jpg, heic or png")
	}
	return produce(src, suffixedName(src, "optimized"), func(tmp string) error {
		if err := write(tmp); err != nil {
			return err
		}
		if size := fileSize(tmp); size > 0 && size >= fileSize(src) {
			return skipError("already optimized")
		}
		return nil
	})
}

func isLossyImage(path string) bool {
	ext := normalExt(path)
	return ext == "jpg" || ext == "heic"
}

// pngOptimizer returns the best installed PNG optimizer: pngquant (lossy, much smaller), oxipng (lossless), or "".
func pngOptimizer() string {
	for _, tool := range []string{"pngquant", "oxipng"} {
		if _, err := exec.LookPath(tool); err == nil {
			return tool
		}
	}
	return ""
}

func optimizePNG(tool, src, out string) error {
	if tool == "oxipng" {
		return runCmd("oxipng", "--opt", "2", "--strip", "safe", "--out", out, "--", src)
	}
	output, err := exec.Command("pngquant", "--quality=65-90", "--speed", "3", "--force", "--output", out, "--", src).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && (exitErr.ExitCode() == 98 || exitErr.ExitCode() == 99) {
		return skipError("already optimized") // 98: result larger, 99: quality would drop too much
	}
	if err != nil {
		return fmt.Errorf("pngquant: %s", cmp.Or(strings.TrimSpace(string(output)), err.Error()))
	}
	return nil
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ---- remove background ----

// removeBackground cuts out the subject with the Vision framework (macOS 14+) into a transparent png.
func removeBackground(src string) (string, error) {
	if !isSipsImage(src) {
		return "", skipError("not an image")
	}
	stem, _ := splitExt(filepath.Base(src))
	return produce(src, stem+" (no bg).png", func(tmp string) error {
		_, err := runJXA(removeBackgroundScript, src, tmp)
		if err != nil && strings.Contains(err.Error(), "no subject found") {
			return skipError("no subject found")
		}
		return err
	})
}

// runJXA runs a JavaScript for Automation script and returns its result, turning osascript's
// "execution error: Error: <message> (-2700)" into just the message.
func runJXA(script string, args ...string) (string, error) {
	out, err := exec.Command("osascript", append([]string{"-l", "JavaScript", "-e", script}, args...)...).CombinedOutput()
	result := strings.TrimSpace(string(out))
	if err != nil {
		if i := strings.LastIndex(result, "Error: "); i >= 0 { // thrown errors come out as "Error: Error: msg"
			result = strings.TrimSuffix(strings.TrimSpace(result[i+len("Error: "):]), " (-2700)")
		}
		return "", errors.New(cmp.Or(result, err.Error()))
	}
	return result, nil
}

// ---- trim ----

func (m *model) startTrim(paths []string) tea.Cmd {
	if len(paths) != 1 {
		m.setErr(errors.New("trim works on one file"))
		return nil
	}
	src := paths[0]
	if !mediaFiles[strings.ToLower(filepath.Ext(src))] || !isRegular(src) {
		m.setErr(fmt.Errorf("%s is not a video or audio file", filepath.Base(src)))
		return nil
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		m.setErr(errors.New("trim needs ffmpeg: brew install ffmpeg"))
		return nil
	}

	duration := mediaDuration(src)
	title := "Trim " + filepath.Base(src)
	if duration > 0 {
		title += " (" + formatStamp(duration, ":") + ")"
	}
	cmd := m.openPrompt(title+", start-end", "", 0, func(s string) tea.Cmd {
		if s == "" {
			return nil
		}
		start, end, err := parseRange(s, duration)
		if err != nil {
			m.setErr(err)
			return nil
		}
		return m.trim(src, start, end)
	})
	m.prompt.input.Placeholder = "0:10-1:30, 1:30- (to the end), -0:45 (from the start)"
	return cmd
}

// trim cuts src from start to end (end 0 = to the end) with stream copy: instant and lossless, but video
// starts at the keyframe before start.
func (m *model) trim(src string, start, end float64) tea.Cmd {
	m.clearSelection()
	j := &job{label: "Trimming " + filepath.Base(src)}
	m.jobs = append(m.jobs, j)
	activeDir := m.active().path

	stem, ext := splitExt(filepath.Base(src))
	endLabel := "end"
	if end > 0 {
		endLabel = formatStamp(end, ".")
	}
	name := fmt.Sprintf("%s (%s-%s)%s", stem, formatStamp(start, "."), endLabel, ext)

	return func() tea.Msg {
		dest, err := produce(src, name, func(tmp string) error {
			args := []string{"-nostdin", "-v", "error", "-y", "-ss", ffmpegTime(start), "-i", src}
			if end > 0 {
				args = append(args, "-t", ffmpegTime(end-start))
			}
			args = append(args, "-map", "0", "-dn", "-c", "copy", "-avoid_negative_ts", "make_zero", tmp)
			return runCmd("ffmpeg", args...)
		})
		done := opDoneMsg{job: j, err: err}
		if err == nil {
			done.status = "Trimmed to " + filepath.Base(dest)
			done.undo = &undoStep{desc: "trim", created: []string{dest}}
			if filepath.Dir(dest) == activeDir {
				done.selectName = filepath.Base(dest)
			}
		}
		return done
	}
}

// mediaDuration returns the duration in seconds via ffprobe, or 0 if unknown.
func mediaDuration(path string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}

// parseRange parses "start-end" where either side may be empty: "1:30-" runs to the end, "-0:45" from the start.
// end is 0 when it runs to the end. duration 0 means unknown and skips the bounds checks.
func parseRange(s string, duration float64) (start, end float64, err error) {
	from, to, ok := strings.Cut(strings.ReplaceAll(s, " ", ""), "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid range %q, use start-end like 0:10-1:30", s)
	}
	if from != "" {
		if start, err = parseTimestamp(from); err != nil {
			return 0, 0, err
		}
	}
	if to != "" {
		if end, err = parseTimestamp(to); err != nil {
			return 0, 0, err
		}
		if end <= start {
			return 0, 0, errors.New("end must be after start")
		}
	}
	if duration > 0 {
		if start >= duration {
			return 0, 0, fmt.Errorf("start %s is past the end (%s)", formatStamp(start, ":"), formatStamp(duration, ":"))
		}
		if end >= duration {
			end = 0
		}
	}
	if start == 0 && end == 0 {
		return 0, 0, errors.New("the range covers the whole file")
	}
	return start, end, nil
}

// parseTimestamp parses "90", "1:30", "1:02:03" and fractions like "1:30.5" into seconds.
func parseTimestamp(s string) (float64, error) {
	invalid := fmt.Errorf("invalid time %q, use 90, 1:30 or 1:02:03", s)
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, invalid
	}
	var total float64
	for i, p := range parts {
		last := i == len(parts)-1
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || (!last && v != math.Trunc(v)) || (i > 0 && v >= 60) {
			return 0, invalid
		}
		total = total*60 + v
	}
	return total, nil
}

// formatStamp formats seconds as "1:30" or "1:02:03" with sep between the parts; tenths are kept if present.
func formatStamp(sec float64, sep string) string {
	tenths := int(math.Round(sec * 10))
	whole, frac := tenths/10, tenths%10
	h, m, s := whole/3600, whole/60%60, whole%60
	out := fmt.Sprintf("%d%s%02d", m, sep, s)
	if h > 0 {
		out = fmt.Sprintf("%d%s%02d%s%02d", h, sep, m, sep, s)
	}
	if frac > 0 {
		out += fmt.Sprintf(".%d", frac)
	}
	return out
}

func ffmpegTime(sec float64) string {
	return strconv.FormatFloat(sec, 'f', 3, 64)
}
