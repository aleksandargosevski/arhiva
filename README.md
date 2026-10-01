# arhiva

> **Note:** This project is entirely vibe-coded for personal use. It works for my workflow but may have rough edges. Use at your own risk, PRs welcome.

A fast Miller-columns file manager for the macOS terminal: Finder-like columns that open to the right as you go deeper, with previews, fuzzy finding, zoxide, bookmarks and vim-style keys.

- Single binary, macOS (Apple Silicon and Intel)
- Columns share the width equally; a preview column shows folders, syntax-highlighted text, images (kitty graphics in Ghostty/kitty, also through tmux), archive contents and media metadata
- Sidebar with bookmarks, each with an automatic `g`+letter jump key
- Fuzzy finder over the current folder or zoxide, content search with ripgrep
- Selection (space, vim-like visual mode), copy/cut/paste with progress, trash, rename and bulk rename in `$EDITOR`, zip/extract, undo
- Filter, sort, tabs, jump history, Finder tags, Quick Look
- Custom shell commands, picker mode for nvim, config that reloads on save

## Installation

### Homebrew

```sh
brew install aleksandargosevski/tap/arhiva
```

### Script

```sh
curl -fsSL https://raw.githubusercontent.com/aleksandargosevski/arhiva/main/install.sh | bash
```

Installs to `/usr/local/bin` by default. Override with `ARHIVA_INSTALL_DIR`:

```sh
curl -fsSL https://raw.githubusercontent.com/aleksandargosevski/arhiva/main/install.sh | ARHIVA_INSTALL_DIR=~/.local/bin bash
```

### From source

```sh
git clone https://github.com/aleksandargosevski/arhiva.git
cd arhiva
make install
```

### Recommended

```sh
brew install fd ripgrep zoxide
```

- [fd](https://github.com/sharkdp/fd): much faster file finder (`f`) that respects `.gitignore`; without it a built-in walk is used
- [ripgrep](https://github.com/BurntSushi/ripgrep): content search (`F`), required for it
- [zoxide](https://github.com/ajeetdsouza/zoxide): the zoxide finder (`z`); every folder you enter is added to it
- a [Nerd Font](https://www.nerdfonts.com) for icons (`icons = true`)
- `git` for status marks in columns (comes with Xcode Command Line Tools)

Moving to Trash uses the built-in `trash` command of macOS 15+.

## Usage

```sh
arhiva                   # opens default_dir from the config (current folder by default)
arhiva ~/Downloads       # a folder
arhiva notes/todo.md     # a file: opens its folder with the cursor on it
arhiva --version
```

Press `?` for the searchable list of all keys.

## Shell integration (cd on quit)

Add to `~/.zshrc`:

```zsh
a() {
  local tmp="$(mktemp -t arhiva)"
  arhiva --cd-file="$tmp" "$@"
  local dir="$(cat "$tmp")"
  rm -f "$tmp"
  [ -n "$dir" ] && [ "$dir" != "$PWD" ] && cd "$dir"
}
```

`q` quits and cds, `Q` / `ctrl+c` quits without cd. Every folder you enter is added to zoxide.

## Picker mode (nvim)

`arhiva --choose-file=FILE` works as a file picker: opening a file (`l`/Enter) writes the chosen
path(s) to FILE and quits. With a selection, all selected paths are written, one per line.

A floating picker for nvim, e.g. in `~/.config/nvim/lua/arhiva.lua`:

```lua
local M = {}

function M.open()
  local chooser = vim.fn.tempname()
  local current = vim.fn.expand("%:p")
  if current == "" then current = vim.fn.getcwd() end

  local w, h = math.floor(vim.o.columns * 0.9), math.floor(vim.o.lines * 0.9)
  local buf = vim.api.nvim_create_buf(false, true)
  local win = vim.api.nvim_open_win(buf, true, {
    relative = "editor", width = w, height = h, border = "rounded",
    row = math.floor((vim.o.lines - h) / 2), col = math.floor((vim.o.columns - w) / 2),
  })

  vim.fn.jobstart({ "arhiva", "--choose-file=" .. chooser, current }, {
    term = true,
    on_exit = function()
      vim.api.nvim_win_close(win, true)
      if vim.fn.filereadable(chooser) == 1 then
        for _, path in ipairs(vim.fn.readfile(chooser)) do
          vim.cmd.edit(vim.fn.fnameescape(path))
        end
        vim.fn.delete(chooser)
      end
    end,
  })
  vim.cmd.startinsert()
end

return M
```

```lua
vim.keymap.set("n", "<leader>e", function() require("arhiva").open() end, { desc = "arhiva" })
```

On nvim older than 0.11 use `vim.fn.termopen(cmd, opts)` instead of `jobstart` with `term = true`.
Inside nvim's terminal images fall back to half-blocks, since it doesn't pass graphics through.

## Config

`~/.config/arhiva/config.toml` (or `$XDG_CONFIG_HOME/arhiva/config.toml`). All fields optional.
Changes apply as soon as the file is saved, no restart needed.

```toml
default_dir    = "."      # "." = folder arhiva was started from; "~" or any path for a fixed start
show_sidebar   = true
show_hidden    = false
icons          = true     # needs a Nerd Font
image_protocol = "auto"   # auto | kitty | blocks
syntax_theme   = "monokai" # any chroma style: dracula, nord, github-dark, catppuccin-mocha…
sort           = "name"   # name | modified | size | ext
git            = false    # git status marks next to entries

# Keys are assigned automatically: "g" + first free letter of the folder name (Sites -> gs).
# `m` adds or removes the folder under the cursor, `d` in the focused sidebar removes one;
# both rewrite only this line and keep the rest of the file.
bookmarks = ["~", "~/Desktop", "~/Downloads"]

# Override any action; string or list. Press ? in the app to see all actions.
[keys]
down = ["j", "down"]
toggle_sidebar = "b"

# Custom commands, run with `sh -c` in the current folder. Available:
#   $f   entry under the cursor     $d  current folder
#   "$@" targets (selection, or entry under cursor), $fx the same, newline separated
# interactive = true hands the terminal over (lazygit, less, nvim); otherwise it runs in the
# background and its last output line is shown in the status line.
[[commands]]
key  = "gc"
desc = "Open in VS Code"
run  = 'code "$f"'

[[commands]]
key  = "gl"
desc = "lazygit"
run  = "lazygit"
interactive = true
```

Custom command keys override built-in ones.

Actions: `up down left right top bottom open page_up page_down jump_back jump_forward set_root new_tab close_tab next_tab prev_tab toggle_sidebar focus toggle_hidden filter sort_name sort_modified sort_size sort_ext sort_reverse find grep zoxide goto select visual invert_selection new copy cut paste zip rename rename_append rename_replace preview_down preview_up quick_look dir_size tags duplicate copy_contents airdrop symlink diff command undo yank trash delete bookmark help quit quit_no_cd`.

## Navigation

| key | action |
|-----|--------|
| `/` | filter the current folder while typing (smart case); enter keeps it, `esc` clears |
| `l` / `enter` | `l` only enters folders; `enter` also opens files |
| `F` | search file contents with ripgrep, preview of the hit on the right; `enter` jumps to the file |
| `gp` | go to a typed or pasted path; lists subfolders as you type, `tab` completes |
| `,n` `,m` `,s` `,e` | sort by name / modified / size / extension; `,r` reverses |
| `ctrl+o` or `[` / `]` | jump back / forward (bookmark, finder and zoxide jumps restore all columns) |
| `t` / `ctrl+w` | new tab / close tab; `gt` / `gT` next / previous, `1`-`9` go to tab |
| `J` / `K` | scroll the preview |
| `i` | Quick Look |
| `S` | calculate folder size (shown in the info line) |

Tagged entries show Finder-colored dots; all tags are listed in the info line.
Prompts and confirmations open as a centered dialog.

Folders refresh on their own when something changes on disk (checked twice a second).
The status line shows size, permissions and modified time of the entry under the cursor.

## Selection

- `space` toggles the entry under the cursor and moves down
- `v` starts a visual range; move with `j`/`k`, `space` selects the range (or unselects it if it was fully selected), `v`/`esc` cancels
- `ctrl+r` inverts the selection in the current folder
- `esc` cancels visual mode, then clears the filter, then clears the selection

The selection holds absolute paths, so it survives moving between folders.

## File operations

Operations act on the selection (plus an active visual range), or on the entry under the cursor.

| key | action |
|-----|--------|
| `n` | new file; end with `/` for a folder, nested paths like `a/b.txt` work |
| `y` / `x` | copy / cut |
| `p` | paste into the current folder; conflicts become `name (1).ext`; progress is shown in the status line |
| `Z` | zip into the current folder (asks for a name); on archives it extracts instead |
| `d` | move to Trash (asks y/N); in the focused sidebar removes the bookmark |
| `D` | delete permanently, `rm -rf` (asks y/N) |
| `rr` / `rf` / `rn` | rename: cursor before extension / at end / empty name, extension kept; with several targets, bulk rename in `$EDITOR` (one name per line) |
| `c` | duplicate next to the original as `name (1).ext` |
| `C` | copy contents to the clipboard: a text file as text, anything else as files (paste as attachments in Slack, Mail, Messages, or as files in Finder) |
| `A` | share via AirDrop (opens the system AirDrop panel) |
| `L` | symlink the copied items into the current folder |
| `=` | diff exactly 2 selected items (`nvim -d` if that's your editor, otherwise `git diff --no-index` in less) |
| `T` | edit Finder tags (comma separated; Red, Orange, Yellow, Green, Blue, Purple, Gray get their Finder color) |
| `:` | run a shell command, same variables as custom commands |
| `u` | undo the last rename, move, paste, new, zip or extract (copies and created files go to Trash) |
| `Y` | copy path(s) to the system clipboard |
| `H` | make the current folder the first column |

Extraction uses bsdtar (zip, tar.*, 7z, rar, iso, …) and gzip/bzip2/xz/zstd for single compressed files.
An archive with one top-level entry is extracted in place, otherwise into a folder named after it.

After the first key of a sequence (`g`, `r`), a panel at the bottom lists what can follow.

## Preview

A preview column on the right shows the entry under the cursor:

- folders: their contents
- archives: the list of files inside (bsdtar, nothing is extracted)
- audio / video: duration, codecs, bitrate… (Spotlight metadata via `mdls`); mp4/mov also show a frame
- photos, pdf: the image plus camera, exposure, pages, download source…
- text: syntax highlighted (chroma), binary files are detected
- png, jpg, gif, webp, bmp, tiff: decoded directly
- svg, pdf, heic, avif, psd, ico…: rendered by macOS QuickLook (`qlmanage`)

`l`/Enter opens text files in `$VISUAL`/`$EDITOR` (inside the terminal), everything else with `open`.

Images use the kitty graphics protocol (Ghostty, kitty; works inside tmux with `set -g allow-passthrough on`), otherwise colored half-blocks.

## Development

```sh
make build   # bin/arhiva
make test
make lint    # gofmt + go vet
make dev     # build and run
```

Releases: push a `v*` tag; GitHub Actions runs GoReleaser, which publishes the macOS binaries and
updates the Homebrew formula in `aleksandargosevski/homebrew-tap` (needs the `HOMEBREW_TAP_TOKEN` secret).

## Built with

- [bubbletea](https://github.com/charmbracelet/bubbletea), [bubbles](https://github.com/charmbracelet/bubbles), [lipgloss](https://github.com/charmbracelet/lipgloss): TUI
- [chroma](https://github.com/alecthomas/chroma): syntax highlighting
- [sahilm/fuzzy](https://github.com/sahilm/fuzzy): fuzzy matching
- [BurntSushi/toml](https://github.com/BurntSushi/toml): config

## License

MIT
