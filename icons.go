package main

import (
	"path/filepath"
	"strings"
)

// Nerd Font glyphs.
const (
	iconFolder   = "\uf07b"
	iconFile     = "\uf15b"
	iconBookmark = "\uf02e"
)

var nameIcons = map[string]string{
	"go.mod":       "\ue627",
	"go.sum":       "\ue627",
	"package.json": "\ue71e",
	"makefile":     "\ue779",
	"dockerfile":   "\uf308",
	".gitignore":   "\ue702",
	"readme.md":    "\uf48a",
}

var extIcons = map[string]string{
	".go":   "\ue627",
	".js":   "\ue74e",
	".mjs":  "\ue74e",
	".jsx":  "\ue7ba",
	".ts":   "\ue628",
	".tsx":  "\ue7ba",
	".vue":  "\ue6a0",
	".json": "\ue60b",
	".md":   "\ue609",
	".py":   "\ue606",
	".rs":   "\ue7a8",
	".rb":   "\ue739",
	".php":  "\ue73d",
	".html": "\ue736",
	".css":  "\ue749",
	".scss": "\ue749",
	".sh":   "\uf489",
	".zsh":  "\uf489",
	".yml":  "\ue615",
	".yaml": "\ue615",
	".toml": "\ue615",
	".lock": "\uf023",
	".txt":  "\uf15c",
	".pdf":  "\uf1c1",
	".png":  "\uf1c5",
	".jpg":  "\uf1c5",
	".jpeg": "\uf1c5",
	".gif":  "\uf1c5",
	".svg":  "\uf1c5",
	".webp": "\uf1c5",
	".mp4":  "\uf1c8",
	".mov":  "\uf1c8",
	".mkv":  "\uf1c8",
	".mp3":  "\uf1c7",
	".wav":  "\uf1c7",
	".flac": "\uf1c7",
	".zip":  "\uf1c6",
	".tar":  "\uf1c6",
	".gz":   "\uf1c6",
	".dmg":  "\uf1c6",
	".app":  "\uf179",
}

var bookmarkIcons = map[string]string{
	"home":      "\uf015",
	"desktop":   "\uf108",
	"documents": "\uf0f6",
	"downloads": "\uf019",
}

func entryIcon(e entry) string {
	if e.isDir {
		return iconFolder
	}
	if icon, ok := nameIcons[strings.ToLower(e.name)]; ok {
		return icon
	}
	if icon, ok := extIcons[strings.ToLower(filepath.Ext(e.name))]; ok {
		return icon
	}
	return iconFile
}

func bookmarkIcon(name string) string {
	if icon, ok := bookmarkIcons[strings.ToLower(name)]; ok {
		return icon
	}
	return iconBookmark
}
