package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/sys/unix"
)

// Finder tags live in this xattr as a binary plist array of "Name\nColorIndex" strings.
const tagsXattr = "com.apple.metadata:_kMDItemUserTags"

// Finder's color indices: 0 none, 1 gray, 2 green, 3 purple, 4 blue, 5 yellow, 6 red, 7 orange.
var (
	tagColors   = []lipgloss.Color{"", "245", "114", "141", "75", "221", "203", "214"}
	tagColorIdx = map[string]int{"gray": 1, "grey": 1, "green": 2, "purple": 3, "blue": 4, "yellow": 5, "red": 6, "orange": 7}
)

type tag struct {
	name  string
	color int
}

func (t tag) String() string { return t.name + "\n" + strconv.Itoa(t.color) }

func readTags(path string) []tag {
	size, err := unix.Getxattr(path, tagsXattr, nil)
	if err != nil || size <= 0 {
		return nil
	}
	buf := make([]byte, size)
	if size, err = unix.Getxattr(path, tagsXattr, buf); err != nil {
		return nil
	}
	strs, err := parseBplistStrings(buf[:size])
	if err != nil {
		return nil
	}
	tags := make([]tag, 0, len(strs))
	for _, s := range strs {
		name, color, _ := strings.Cut(s, "\n")
		c, _ := strconv.Atoi(color)
		tags = append(tags, tag{name, clamp(c, 0, len(tagColors)-1)})
	}
	return tags
}

func writeTags(path string, tags []tag) error {
	if len(tags) == 0 {
		err := unix.Removexattr(path, tagsXattr)
		if errors.Is(err, unix.ENOATTR) {
			return nil
		}
		return err
	}
	strs := make([]string, len(tags))
	for i, t := range tags {
		strs[i] = t.String()
	}
	return unix.Setxattr(path, tagsXattr, encodeBplistStrings(strs), 0)
}

// startTags edits the tags common to all targets; tags only some targets have are left alone.
func (m *model) startTags() tea.Cmd {
	paths := m.targets()
	if len(paths) == 0 {
		return nil
	}
	current := make([][]tag, len(paths))
	for i, p := range paths {
		current[i] = readTags(p)
	}
	common := tagNames(current[0])
	for _, ts := range current[1:] {
		names := tagNames(ts)
		common = slices.DeleteFunc(common, func(n string) bool { return !slices.Contains(names, n) })
	}
	value := strings.Join(common, ", ")
	return m.openPrompt("Tags for "+countLabel(paths)+"  (comma separated; Red, Blue… get Finder colors)", value, len([]rune(value)), func(input string) tea.Cmd {
		var wanted []string
		for _, n := range strings.Split(input, ",") {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(wanted, n) {
				wanted = append(wanted, n)
			}
		}
		colorOf := knownColors(current)
		var errs []error
		for i, p := range paths {
			next := slices.DeleteFunc(slices.Clone(current[i]), func(t tag) bool {
				return slices.Contains(common, t.name) && !slices.Contains(wanted, t.name)
			})
			for _, n := range wanted {
				if !slices.Contains(tagNames(next), n) {
					next = append(next, tag{n, colorOf(n)})
				}
			}
			if err := writeTags(p, next); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", tildePath(p), err))
			}
		}
		m.clearSelection()
		m.afterOp("")
		if err := errors.Join(errs...); err != nil {
			m.setErr(err)
		} else {
			m.status, m.statusIsErr = "Tagged "+countLabel(paths), false
		}
		return nil
	})
}

// knownColors picks a new tag's color: the one it already has on another target, else Finder's
// standard color for names like "Red", else none.
func knownColors(current [][]tag) func(string) int {
	seen := map[string]int{}
	for _, ts := range current {
		for _, t := range ts {
			seen[t.name] = t.color
		}
	}
	return func(name string) int {
		if c, ok := seen[name]; ok {
			return c
		}
		return tagColorIdx[strings.ToLower(name)]
	}
}

func tagNames(tags []tag) []string {
	names := make([]string, len(tags))
	for i, t := range tags {
		names[i] = t.name
	}
	return names
}

// tagDots renders colored tags as Finder-like dots (colorless tags are only listed in the info line).
func tagDots(tags []tag) (string, int) {
	var b strings.Builder
	n := 0
	for _, t := range tags {
		if t.color == 0 || n == 3 {
			continue
		}
		b.WriteString(lipgloss.NewStyle().Foreground(tagColors[t.color]).Render("●"))
		n++
	}
	return b.String(), n
}

// ---- minimal binary plist (bplist00): a top-level array of strings, which is all tags need ----

func parseBplistStrings(b []byte) ([]string, error) {
	errBad := errors.New("unsupported plist")
	if len(b) < 40 || string(b[:8]) != "bplist00" {
		return nil, errBad
	}
	trailer := b[len(b)-32:]
	offSize, refSize := int(trailer[6]), int(trailer[7])
	numObjects := binary.BigEndian.Uint64(trailer[8:16])
	top := binary.BigEndian.Uint64(trailer[16:24])
	offTable := binary.BigEndian.Uint64(trailer[24:32])
	readUint := func(pos uint64, n int) (uint64, bool) {
		if n < 1 || n > 8 || pos+uint64(n) > uint64(len(b)) {
			return 0, false
		}
		var v uint64
		for _, c := range b[pos : pos+uint64(n)] {
			v = v<<8 | uint64(c)
		}
		return v, true
	}
	// object returns the marker nibble, element count and where the payload starts.
	object := func(ref uint64) (kind byte, count, start uint64, ok bool) {
		if ref >= numObjects {
			return 0, 0, 0, false
		}
		off, ok := readUint(offTable+ref*uint64(offSize), offSize)
		if !ok || off >= uint64(len(b)) {
			return 0, 0, 0, false
		}
		kind, count, start = b[off]>>4, uint64(b[off]&0xF), off+1
		if count == 0xF {
			if start >= uint64(len(b)) || b[start]>>4 != 0x1 {
				return 0, 0, 0, false
			}
			n := 1 << (b[start] & 0xF)
			if count, ok = readUint(start+1, n); !ok {
				return 0, 0, 0, false
			}
			start += 1 + uint64(n)
		}
		return kind, count, start, true
	}

	kind, count, start, ok := object(top)
	if !ok || kind != 0xA {
		return nil, errBad
	}
	var out []string
	for i := range count {
		ref, ok := readUint(start+i*uint64(refSize), refSize)
		if !ok {
			return nil, errBad
		}
		k, n, s, ok := object(ref)
		switch {
		case !ok:
			return nil, errBad
		case k == 0x5 && s+n <= uint64(len(b)):
			out = append(out, string(b[s:s+n]))
		case k == 0x6 && s+2*n <= uint64(len(b)):
			units := make([]uint16, n)
			for j := range units {
				units[j] = binary.BigEndian.Uint16(b[s+2*uint64(j):])
			}
			out = append(out, string(utf16.Decode(units)))
		default:
			return nil, errBad
		}
	}
	return out, nil
}

func encodeBplistStrings(strs []string) []byte {
	buf := []byte("bplist00")
	var offsets []int
	writeHeader := func(kind byte, n int) {
		if n < 15 {
			buf = append(buf, kind<<4|byte(n))
			return
		}
		buf = append(buf, kind<<4|0xF)
		if n < 256 {
			buf = append(buf, 0x10, byte(n))
		} else {
			buf = append(buf, 0x11, byte(n>>8), byte(n))
		}
	}
	refSize := 1
	if len(strs)+1 > 255 {
		refSize = 2
	}

	offsets = append(offsets, len(buf))
	writeHeader(0xA, len(strs))
	for i := range strs {
		ref := i + 1
		if refSize == 2 {
			buf = append(buf, byte(ref>>8))
		}
		buf = append(buf, byte(ref))
	}
	for _, s := range strs {
		offsets = append(offsets, len(buf))
		if isASCII(s) {
			writeHeader(0x5, len(s))
			buf = append(buf, s...)
			continue
		}
		units := utf16.Encode([]rune(s))
		writeHeader(0x6, len(units))
		for _, u := range units {
			buf = append(buf, byte(u>>8), byte(u))
		}
	}

	offTable := len(buf)
	offSize := 1
	for offTable >= 1<<(8*offSize) {
		offSize *= 2
	}
	for _, off := range offsets {
		for i := offSize - 1; i >= 0; i-- {
			buf = append(buf, byte(off>>(8*i)))
		}
	}
	trailer := make([]byte, 32)
	trailer[6], trailer[7] = byte(offSize), byte(refSize)
	binary.BigEndian.PutUint64(trailer[8:], uint64(len(offsets)))
	binary.BigEndian.PutUint64(trailer[16:], 0)
	binary.BigEndian.PutUint64(trailer[24:], uint64(offTable))
	return append(buf, trailer...)
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
