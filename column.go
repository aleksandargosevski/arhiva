package main

import (
	"cmp"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type entry struct {
	name  string
	isDir bool
	meta  *entryMeta // shared by copies of the entry; filled lazily, see loadMeta
}

// entryMeta costs a syscall or two per entry (lstat, getxattr), about 4s for 50k files, so it is
// only read for what is on screen, or for everything when sorting needs it.
type entryMeta struct {
	statOnce, tagsOnce sync.Once
	size               int64
	modTime            time.Time
	mode               fs.FileMode // from lstat, so symlinks keep ModeSymlink
	tags               []tag
}

// listOpts controls which entries a column shows and in what order.
type listOpts struct {
	showHidden bool
	sortBy     string // name, modified, size, ext
	reverse    bool
}

type column struct {
	path    string
	all     []entry // every entry, sorted
	entries []entry // all, narrowed by filter
	filter  string
	cursor  int
	offset  int
	modTime time.Time // of the folder itself, to notice changes
}

var sortModes = []string{"name", "modified", "size", "ext"}

func loadColumn(path string, opts listOpts) (*column, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]entry, 0, len(dirEntries))
	for _, de := range dirEntries {
		name := de.Name()
		if !opts.showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		e := entry{name: name, isDir: de.IsDir(), meta: &entryMeta{}}
		if de.Type()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(filepath.Join(path, name)); err == nil {
				e.isDir = target.IsDir()
			}
		}
		entries = append(entries, e)
	}
	if opts.sortBy == "modified" || opts.sortBy == "size" {
		loadAllStats(path, entries)
	}
	sortEntries(entries, opts)
	col := &column{path: path, all: entries, modTime: info.ModTime()}
	col.entries = entries
	return col, nil
}

// loadMeta reads the entry's metadata once; safe to call from several goroutines.
func loadMeta(dir string, e entry) *entryMeta {
	loadStat(dir, e)
	e.meta.tagsOnce.Do(func() { e.meta.tags = readTags(filepath.Join(dir, e.name)) })
	return e.meta
}

// loadStat reads only what sorting needs; tags are the expensive part and aren't needed for it.
func loadStat(dir string, e entry) {
	m := e.meta
	m.statOnce.Do(func() {
		if fi, err := os.Lstat(filepath.Join(dir, e.name)); err == nil {
			m.size, m.modTime, m.mode = fi.Size(), fi.ModTime(), fi.Mode()
		}
	})
}

func loadAllStats(dir string, entries []entry) {
	jobs := make(chan entry)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range jobs {
				loadStat(dir, e)
			}
		}()
	}
	for _, e := range entries {
		jobs <- e
	}
	close(jobs)
	wg.Wait()
}

// sortEntries keeps folders first; "modified" and "size" put newest/largest first.
func sortEntries(entries []entry, opts listOpts) {
	byName := func(a, b entry) int { return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) }
	slices.SortStableFunc(entries, func(a, b entry) int {
		if a.isDir != b.isDir {
			if a.isDir {
				return -1
			}
			return 1
		}
		var c int
		switch opts.sortBy {
		case "modified": // meta is preloaded for these two sorts
			c = b.meta.modTime.Compare(a.meta.modTime)
		case "size":
			if !a.isDir { // folder sizes from lstat are meaningless
				c = cmp.Compare(b.meta.size, a.meta.size)
			}
		case "ext":
			c = strings.Compare(strings.ToLower(filepath.Ext(a.name)), strings.ToLower(filepath.Ext(b.name)))
		}
		c = cmp.Or(c, byName(a, b))
		if opts.reverse {
			return -c
		}
		return c
	})
}

// setFilter narrows entries to names containing query (smart case: case-sensitive only if query has uppercase).
// The cursor stays on the same entry when it still matches.
func (c *column) setFilter(query string) {
	cur, hadCur := c.selected()
	c.filter = query
	c.entries = c.all
	if query != "" {
		match := strings.Contains
		if strings.ToLower(query) == query {
			match = func(name, q string) bool { return strings.Contains(strings.ToLower(name), q) }
		}
		c.entries = nil
		for _, e := range c.all {
			if match(e.name, query) {
				c.entries = append(c.entries, e)
			}
		}
	}
	c.cursor = 0
	if hadCur {
		c.selectName(cur.name)
	}
}

func (c *column) selected() (entry, bool) {
	if len(c.entries) == 0 {
		return entry{}, false
	}
	return c.entries[c.cursor], true
}

func (c *column) selectName(name string) bool {
	for i, e := range c.entries {
		if e.name == name {
			c.cursor = i
			return true
		}
	}
	return false
}

func (c *column) scrollTo(rows int) {
	if c.cursor < c.offset {
		c.offset = c.cursor
	}
	if c.cursor >= c.offset+rows {
		c.offset = c.cursor - rows + 1
	}
	c.offset = clamp(c.offset, 0, max(len(c.entries)-rows, 0))
}
