package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

var version = "dev" // set by -ldflags at release

func main() {
	cdFile := flag.String("cd-file", "", "on quit (q), write the current folder to this file so a shell wrapper can cd into it")
	chooseFile := flag.String("choose-file", "", "picker mode: opening a file writes the chosen path(s) to this file and quits")
	showVersion := flag.Bool("version", false, "print version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: arhiva [flags] [folder or file]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("arhiva", version)
		return
	}

	cfg, cfgWarn := loadConfig()
	start := cfg.DefaultDir
	if flag.NArg() > 0 {
		start = expandHome(flag.Arg(0))
	}
	start, err := filepath.Abs(start)
	if err != nil {
		fail(err)
	}
	// A file argument opens its folder with the cursor on it.
	selectName := ""
	if info, err := os.Stat(start); err == nil && !info.IsDir() {
		start, selectName = filepath.Dir(start), filepath.Base(start)
	}

	m, err := newModel(cfg, start)
	if err != nil {
		fail(err)
	}
	if strings.HasPrefix(selectName, ".") && !m.showHidden {
		m.toggleHidden()
	}
	m.active().selectName(selectName)
	m.chooseFile = *chooseFile
	if cfgWarn != nil {
		m.setErr(cfgWarn)
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	stopOnHangup(p)
	final, err := p.Run()
	stopAllServers()
	if err != nil {
		fail(err)
	}
	fm := final.(*model)
	fm.closeDragPanel()
	fm.preview.cleanup()
	if fm.cdOnQuit && *cdFile != "" {
		if err := os.WriteFile(*cdFile, []byte(fm.active().path), 0o600); err != nil {
			fail(err)
		}
	}
}

// stopOnHangup ends the program when the terminal is closed, so servers are stopped instead of
// left running (bubbletea itself only handles SIGINT and SIGTERM).
func stopOnHangup(p *tea.Program) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		<-hup
		p.Kill()
	}()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "arhiva:", err)
	os.Exit(1)
}
