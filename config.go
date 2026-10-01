package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/alecthomas/chroma/v2/styles"
)

type Config struct {
	DefaultDir    string             `toml:"default_dir"`
	ShowSidebar   bool               `toml:"show_sidebar"`
	ShowHidden    bool               `toml:"show_hidden"`
	Icons         bool               `toml:"icons"`
	ImageProtocol string             `toml:"image_protocol"` // auto, kitty, blocks
	SyntaxTheme   string             `toml:"syntax_theme"`
	Sort          string             `toml:"sort"` // name, modified, size, ext
	Git           bool               `toml:"git"`
	Bookmarks     []string           `toml:"bookmarks"`
	Commands      []Command          `toml:"commands"`
	Keys          map[string]keyList `toml:"keys"`
}

// Command is a user-defined shell command bound to a key.
type Command struct {
	Key         string `toml:"key"`
	Run         string `toml:"run"`
	Desc        string `toml:"desc"`
	Interactive bool   `toml:"interactive"` // hand the terminal over (lazygit, less, nvim)
}

type Bookmark struct {
	Name, Path, Key string
}

type action struct {
	name, desc string
	keys       []string
}

// Order here is the order shown in the help overlay.
var actions = []action{
	{"up", "Move up", []string{"k", "up"}},
	{"down", "Move down", []string{"j", "down"}},
	{"left", "Back / parent folder", []string{"h", "left", "backspace"}},
	{"right", "Open folder", []string{"l", "right"}},
	{"open", "Open folder / file", []string{"enter"}},
	{"top", "Go to top", []string{"gg"}},
	{"bottom", "Go to bottom", []string{"G"}},
	{"page_up", "Half page up", []string{"ctrl+u"}},
	{"page_down", "Half page down", []string{"ctrl+d"}},
	{"jump_back", "Jump back (before bookmark / finder jump)", []string{"ctrl+o", "["}},
	{"jump_forward", "Jump forward", []string{"]"}},
	{"set_root", "Make current folder the first column", []string{"H"}},
	{"new_tab", "New tab", []string{"t"}},
	{"close_tab", "Close tab", []string{"ctrl+w"}},
	{"next_tab", "Next tab", []string{"gt"}},
	{"prev_tab", "Previous tab", []string{"gT"}},
	{"toggle_sidebar", "Toggle sidebar", []string{"b"}},
	{"focus", "Switch focus sidebar / columns", []string{"tab"}},
	{"toggle_hidden", "Toggle hidden files", []string{"."}},
	{"filter", "Filter current folder", []string{"/"}},
	{"sort_name", "Sort by name", []string{",n"}},
	{"sort_modified", "Sort by modified (newest first)", []string{",m"}},
	{"sort_size", "Sort by size (largest first)", []string{",s"}},
	{"sort_ext", "Sort by extension", []string{",e"}},
	{"sort_reverse", "Reverse sort order", []string{",r"}},
	{"find", "Fuzzy find in current folder", []string{"f"}},
	{"grep", "Search file contents (ripgrep)", []string{"F"}},
	{"zoxide", "Fuzzy find with zoxide", []string{"z"}},
	{"goto", "Go to path (tab completes)", []string{"gp"}},
	{"select", "Select / unselect (confirm in visual)", []string{"space"}},
	{"visual", "Visual mode: select a range", []string{"v"}},
	{"invert_selection", "Invert selection in current folder", []string{"ctrl+r"}},
	{"new", "New file (or folder, ending with /)", []string{"n"}},
	{"copy", "Copy", []string{"y"}},
	{"cut", "Cut", []string{"x"}},
	{"paste", "Paste into current folder", []string{"p"}},
	{"tags", "Edit Finder tags", []string{"T"}},
	{"duplicate", "Duplicate next to the original", []string{"c"}},
	{"copy_contents", "Copy contents to clipboard (text; other files as files)", []string{"C"}},
	{"airdrop", "Share via AirDrop", []string{"A"}},
	{"symlink", "Symlink copied items here", []string{"L"}},
	{"diff", "Diff 2 selected items", []string{"="}},
	{"zip", "Zip, or extract if archives", []string{"Z"}},
	{"trash", "Move to Trash (sidebar: remove bookmark)", []string{"d"}},
	{"delete", "Delete permanently", []string{"D"}},
	{"rename", "Rename (cursor before extension; several: bulk in $EDITOR)", []string{"rr"}},
	{"rename_append", "Rename (cursor at end)", []string{"rf"}},
	{"rename_replace", "Rename (new name, keep extension)", []string{"rn"}},
	{"preview_down", "Scroll preview down", []string{"J"}},
	{"preview_up", "Scroll preview up", []string{"K"}},
	{"quick_look", "Quick Look", []string{"i"}},
	{"dir_size", "Calculate folder size", []string{"S"}},
	{"command", "Run a shell command ($f, $d, \"$@\")", []string{":"}},
	{"undo", "Undo last rename / move / paste / create", []string{"u"}},
	{"yank", "Copy path(s)", []string{"Y"}},
	{"bookmark", "Bookmark / unbookmark folder under cursor", []string{"m"}},
	{"help", "Show keybindings", []string{"?"}},
	{"quit", "Quit and cd to folder", []string{"q"}},
	{"quit_no_cd", "Quit without cd", []string{"Q"}},
}

// keyList accepts both `action = "x"` and `action = ["x", "y"]`.
type keyList []string

func (k *keyList) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case string:
		*k = keyList{v}
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("key must be a string, got %v", item)
			}
			*k = append(*k, s)
		}
	default:
		return fmt.Errorf("keys must be a string or list of strings, got %v", v)
	}
	return nil
}

func defaultConfig() Config {
	return Config{
		DefaultDir:    "~",
		ShowSidebar:   true,
		Icons:         true,
		ImageProtocol: "auto",
		SyntaxTheme:   "monokai",
		Sort:          "name",
		Git:           false,
		Bookmarks:     []string{"~", "~/Desktop", "~/Downloads"},
	}
}

// loadConfig always returns a usable config; the error is a warning to show the user.
func loadConfig() (Config, error) {
	path := configPath()
	cfg := defaultConfig()
	var warn error

	md, err := toml.DecodeFile(path, &cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		cfg, warn = defaultConfig(), fmt.Errorf("config %s: %w", path, err)
	case len(md.Undecoded()) > 0:
		warn = fmt.Errorf("config: unknown fields %v", md.Undecoded())
	}

	if _, ok := styles.Registry[cfg.SyntaxTheme]; !ok {
		warn = errors.Join(warn, fmt.Errorf("config: unknown syntax_theme %q, using monokai", cfg.SyntaxTheme))
		cfg.SyntaxTheme = "monokai"
	}
	if !slices.Contains(sortModes, cfg.Sort) {
		warn = errors.Join(warn, fmt.Errorf("config: sort must be one of %v", sortModes))
		cfg.Sort = "name"
	}
	cfg.DefaultDir = expandHome(cfg.DefaultDir)
	for i, p := range cfg.Bookmarks {
		cfg.Bookmarks[i] = filepath.Clean(expandHome(p))
	}
	return cfg, warn
}

// buildBindings maps key sequences to action names, then gives every bookmark a free
// "g"+letter key (preferring letters from its name) mapped to "bookmark:<index>".
func buildBindings(cfg Config) (map[string]string, []Bookmark, error) {
	known := map[string]bool{}
	bindings := map[string]string{}
	for _, a := range actions {
		known[a.name] = true
		keys := a.keys
		if custom, ok := cfg.Keys[a.name]; ok {
			keys = custom
		}
		for _, k := range keys {
			bindings[k] = a.name
		}
	}
	var problems []string
	for i, c := range cfg.Commands {
		if c.Key == "" || c.Run == "" {
			problems = append(problems, fmt.Sprintf("commands[%d] needs key and run", i))
			continue
		}
		bindings[c.Key] = fmt.Sprintf("cmd:%d", i) // user commands override built-in keys
	}

	bookmarks := make([]Bookmark, len(cfg.Bookmarks))
	for i, path := range cfg.Bookmarks {
		b := Bookmark{Name: bookmarkName(path), Path: path}
		for _, r := range strings.ToLower(b.Name) + "abcdefghijklmnopqrstuvwxyz" {
			key := "g" + string(r)
			if _, taken := bindings[key]; !taken && r >= 'a' && r <= 'z' {
				b.Key = key
				bindings[key] = fmt.Sprintf("bookmark:%d", i)
				break
			}
		}
		bookmarks[i] = b
	}

	var unknown []string
	for name := range cfg.Keys {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		problems = append(problems, "unknown actions in [keys]: "+strings.Join(unknown, ", "))
	}
	if len(problems) > 0 {
		return bindings, bookmarks, fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return bindings, bookmarks, nil
}

func bookmarkName(path string) string {
	if path == homeDir() {
		return "Home"
	}
	return filepath.Base(path)
}

var bookmarksLine = regexp.MustCompile(`(?m)^bookmarks\s*=\s*\[(?:[^\]"']|"(?:[^"\\]|\\.)*"|'[^']*')*\]`)

// saveBookmarks rewrites only the `bookmarks = [...]` entry of the config file, keeping everything else.
func saveBookmarks(paths []string) error {
	path := configPath()
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tilded := make([]string, len(paths))
	for i, p := range paths {
		tilded[i] = tildePath(p)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string][]string{"bookmarks": tilded}); err != nil {
		return err
	}
	line := strings.TrimSpace(buf.String())

	var updated string
	if bookmarksLine.Match(old) {
		updated = bookmarksLine.ReplaceAllLiteralString(string(old), line)
	} else {
		updated = line + "\n" + string(old) // top-level keys must come before any [table]
	}
	var check Config
	if _, err := toml.Decode(updated, &check); err != nil {
		return fmt.Errorf("refusing to write config, result would be invalid: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func configModTime() time.Time {
	info, err := os.Stat(configPath())
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func configPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "arhiva", "config.toml")
	}
	return filepath.Join(homeDir(), ".config", "arhiva", "config.toml")
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return home
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

func tildePath(p string) string {
	home := homeDir()
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}
