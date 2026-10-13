package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestServerCommand(t *testing.T) {
	cmd, err := serverCommand("php -S localhost:$port", 8001)
	must(t, err)
	if got := strings.Join(cmd.Args, " "); got != "sh -c php -S localhost:$port" {
		t.Errorf("configured command: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := serverCommand("", 8001); err == nil || !strings.Contains(err.Error(), "python3 or npx") {
		t.Errorf("without python3 and npx: %v", err)
	}
}

func TestFreePort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port
	if !portInUse(busy) {
		t.Fatal("listening port should be in use")
	}
	port, err := freePort([]int{firstServerPort})
	must(t, err)
	if port == firstServerPort || port < firstServerPort {
		t.Fatalf("taken port %d was returned", port)
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 2}
	io.WriteString(w, "one\ntwo\n")
	io.WriteString(w, "three\nfour")
	if got := strings.Join(w.lines, ","); got != "two,three" {
		t.Errorf("lines %q", got)
	}
	if w.last() != "four" {
		t.Errorf("last %q", w.last())
	}
}

func TestServeAndStop(t *testing.T) {
	if !hasCommand("python3") {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("hello"), 0o644))
	m, err := newModel(Config{ShowSidebar: true, Bookmarks: []string{root}}, root)
	must(t, err)
	t.Cleanup(stopAllServers)

	m.handleKey("S") // the returned batch would open a browser, so its parts are run by hand below
	if len(m.servers) != 1 {
		t.Fatalf("server not started: %s", m.status)
	}
	s := m.servers[0]
	exited := make(chan tea.Msg, 1)
	go func() { exited <- waitServerExit(s)() }()
	if ready := waitServerReady(s)().(serverReadyMsg); !ready.ready {
		state := "still running"
		select {
		case <-s.done:
			state = "exited"
		default:
		}
		t.Fatalf("server not ready (%s), output: %q", state, s.log.last())
	}
	resp, err := http.Get(s.url() + "/index.html")
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("served %q", body)
	}

	m.handleKey("S")
	if len(m.servers) != 1 || !strings.Contains(m.status, "Already serving") {
		t.Fatalf("second S should reuse the server: %d servers, %q", len(m.servers), m.status)
	}

	m.handleKey("tab")
	m.handleKey("j")
	if got, ok := m.sidebarServer(); !ok || got != s {
		t.Fatalf("sidebar cursor %d not on the server", m.sidebarCursor)
	}
	m.handleKey("d")
	select {
	case msg := <-exited:
		m.update(msg)
	case <-time.After(2 * serverStopWait):
		t.Fatal("server did not exit after d")
	}
	if len(m.servers) != 0 || m.statusIsErr {
		t.Fatalf("stop failed: %d servers, %q", len(m.servers), m.status)
	}
	if m.sidebarCursor != 0 {
		t.Fatalf("cursor should move back to the bookmarks, at %d", m.sidebarCursor)
	}
	if portInUse(s.port) {
		t.Fatal("port still in use after stop")
	}
}

func TestServerCrashIsReported(t *testing.T) {
	root := t.TempDir()
	m, err := newModel(Config{Server: "echo booting; echo boom >&2; exit 3"}, root)
	must(t, err)
	m.handleKey("S")
	if len(m.servers) != 1 {
		t.Fatalf("server not started: %s", m.status)
	}
	s := m.servers[0]
	m.update(waitServerExit(s)())
	if !m.statusIsErr || !strings.Contains(m.status, "exited: boom") {
		t.Fatalf("crash not reported: %q", m.status)
	}
	if msg := waitServerReady(s)().(serverReadyMsg); msg.ready || m.serverReady(msg) != nil {
		t.Fatal("ready check after exit should be a no-op")
	}
}

func TestStopAllServers(t *testing.T) {
	root := t.TempDir()
	m, err := newModel(Config{Server: "sleep 60 & wait"}, root)
	must(t, err)
	m.handleKey("S")
	s := m.servers[0]
	go waitServerExit(s)()
	time.Sleep(200 * time.Millisecond) // let sh start sleep, as a real server would be up by now
	// Not asserting that SIGTERM alone was enough: CI runners start processes with SIGTERM blocked,
	// which children inherit, so there the SIGKILL fallback does the work.
	stopAllServers()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("server still running after stopAllServers")
	}
}
