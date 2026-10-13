package main

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	firstServerPort = 8000
	lastServerPort  = 8999
	serverStartWait = 10 * time.Second // npx may download serve on first use
	serverStopWait  = 2 * time.Second
	serverLogLines  = 20
)

// server is a static file server (or the configured command) running in a folder. It lives in its own
// process group so stopping it also stops children (node under npx), and it never outlives arhiva.
type server struct {
	dir      string
	port     int
	cmd      *exec.Cmd
	log      *tailWriter
	done     chan struct{} // closed when the process exited
	stopping bool          // stopped on purpose, so its exit isn't reported as an error
}

type serverReadyMsg struct {
	srv   *server
	ready bool
}

type serverExitedMsg struct {
	srv *server
	err error
}

func (s *server) url() string  { return fmt.Sprintf("http://localhost:%d", s.port) }
func (s *server) name() string { return filepath.Base(s.dir) }

// startServer serves the current folder on the first free port from 8000 and opens it in the browser.
// A folder that is already served is just opened again.
func (m *model) startServer() tea.Cmd {
	dir := m.active().path
	if i := slices.IndexFunc(m.servers, func(s *server) bool { return s.dir == dir }); i >= 0 {
		s := m.servers[i]
		m.status, m.statusIsErr = "Already serving "+s.name()+" at "+s.url(), false
		return openURL(s.url())
	}
	port, err := freePort(m.serverPorts())
	if err != nil {
		m.setErr(err)
		return nil
	}
	cmd, err := serverCommand(m.cfg.Server, port)
	if err != nil {
		m.setErr(err)
		return nil
	}

	s := &server{dir: dir, port: port, cmd: cmd, log: &tailWriter{max: serverLogLines}, done: make(chan struct{})}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "port="+strconv.Itoa(port), "PORT="+strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = s.log, s.log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		m.setErr(fmt.Errorf("starting server: %w", err))
		return nil
	}
	m.servers = append(m.servers, s)
	registerServer(s)
	m.status, m.statusIsErr = fmt.Sprintf("Starting server for %s on :%d…", s.name(), port), false
	return tea.Batch(waitServerExit(s), waitServerReady(s))
}

// pythonServer is `python3 -m http.server --bind 127.0.0.1` without its reverse DNS lookup of the bind
// address, which runs before listening and can stall startup for many seconds on a slow or missing network.
const pythonServer = `import http.server, socketserver, sys

class Server(http.server.ThreadingHTTPServer):
    def server_bind(self):
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]

port = int(sys.argv[1])
server = Server(("127.0.0.1", port), http.server.SimpleHTTPRequestHandler)
print(f"Serving on http://localhost:{port}", flush=True)
server.serve_forever()
`

// serverCommand is the configured `server` command (run with sh -c, $port set), otherwise python3's
// http.server (pythonServer), otherwise npx serve.
func serverCommand(configured string, port int) (*exec.Cmd, error) {
	p := strconv.Itoa(port)
	switch {
	case configured != "":
		return exec.Command("sh", "-c", configured), nil
	case hasCommand("python3"):
		return exec.Command("python3", "-c", pythonServer, p), nil
	case hasCommand("npx"):
		return exec.Command("npx", "--yes", "serve", "--no-clipboard", "-l", "tcp://127.0.0.1:"+p), nil
	}
	return nil, errors.New("serving a folder needs python3 or npx (Node.js), or a `server` command in the config")
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// freePort returns the first port from 8000 that nothing accepts connections on and that can be bound.
func freePort(taken []int) (int, error) {
	for port := firstServerPort; port <= lastServerPort; port++ {
		if slices.Contains(taken, port) || portInUse(port) {
			continue
		}
		l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		l.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port between %d and %d", firstServerPort, lastServerPort)
}

func portInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", "localhost:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (m *model) serverPorts() []int {
	ports := make([]int, len(m.servers))
	for i, s := range m.servers {
		ports[i] = s.port
	}
	return ports
}

func waitServerExit(s *server) tea.Cmd {
	return func() tea.Msg {
		err := s.cmd.Wait()
		close(s.done)
		unregisterServer(s)
		return serverExitedMsg{srv: s, err: err}
	}
}

// waitServerReady polls the port until it accepts connections, the process exits, or it times out.
func waitServerReady(s *server) tea.Cmd {
	return func() tea.Msg {
		deadline := time.Now().Add(serverStartWait)
		for time.Now().Before(deadline) {
			select {
			case <-s.done:
				return serverReadyMsg{srv: s}
			case <-time.After(100 * time.Millisecond):
			}
			if portInUse(s.port) {
				return serverReadyMsg{srv: s, ready: true}
			}
		}
		return serverReadyMsg{srv: s}
	}
}

func (m *model) serverReady(msg serverReadyMsg) tea.Cmd {
	s := msg.srv
	if !slices.Contains(m.servers, s) {
		return nil // exited before it came up; serverExited reports why
	}
	if !msg.ready {
		m.setErr(fmt.Errorf("server for %s started, but nothing answers on :%d yet", s.name(), s.port))
		return nil
	}
	m.status, m.statusIsErr = "Serving "+s.name()+" at "+s.url(), false
	return openURL(s.url())
}

func (m *model) serverExited(msg serverExitedMsg) {
	s := msg.srv
	i := slices.Index(m.servers, s)
	if i < 0 {
		return
	}
	m.servers = slices.Delete(m.servers, i, i+1)
	if m.sidebarFocus && m.sidebarCursor > len(m.bookmarks)+i {
		m.sidebarCursor--
	}
	m.clampSidebar()
	if s.stopping {
		return
	}
	reason := s.log.last()
	if reason == "" && msg.err != nil {
		reason = msg.err.Error()
	}
	m.setErr(fmt.Errorf("server %s :%d exited: %s", s.name(), s.port, cmp.Or(reason, "no output")))
}

func (m *model) stopServer(s *server) {
	s.stop()
	m.status, m.statusIsErr = fmt.Sprintf("Stopped server %s :%d", s.name(), s.port), false
}

// stop sends SIGTERM to the server's process group, and SIGKILL if it's still running after a while.
func (s *server) stop() {
	s.stopping = true
	pgid := -s.cmd.Process.Pid
	syscall.Kill(pgid, syscall.SIGTERM)
	go func() {
		select {
		case <-s.done:
		case <-time.After(serverStopWait):
			syscall.Kill(pgid, syscall.SIGKILL)
		}
	}()
}

// ---- sidebar ----

// sidebarServer is the server under the sidebar cursor; servers are listed after the bookmarks.
func (m *model) sidebarServer() (*server, bool) {
	i := m.sidebarCursor - len(m.bookmarks)
	if !m.sidebarFocus || i < 0 || i >= len(m.servers) {
		return nil, false
	}
	return m.servers[i], true
}

func (m *model) sidebarLen() int { return len(m.bookmarks) + len(m.servers) }

func (m *model) clampSidebar() {
	m.sidebarCursor = clamp(m.sidebarCursor, 0, m.sidebarLen()-1)
	m.sidebarFocus = m.sidebarFocus && m.sidebarLen() > 0
}

// ---- cleanup on exit ----

// liveServers is every started server that hasn't exited, for stopping them when arhiva exits
// for any reason (quit, ctrl+c, SIGTERM, closed terminal).
var liveServers = struct {
	sync.Mutex
	set map[*server]bool
}{set: map[*server]bool{}}

func registerServer(s *server) {
	liveServers.Lock()
	defer liveServers.Unlock()
	liveServers.set[s] = true
}

func unregisterServer(s *server) {
	liveServers.Lock()
	defer liveServers.Unlock()
	delete(liveServers.set, s)
}

// stopAllServers stops every running server and waits until they exited (killed after serverStopWait).
func stopAllServers() {
	liveServers.Lock()
	servers := make([]*server, 0, len(liveServers.set))
	for s := range liveServers.set {
		servers = append(servers, s)
	}
	liveServers.Unlock()

	for _, s := range servers {
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(serverStopWait)
	for _, s := range servers {
		select {
		case <-s.done:
		case <-time.After(time.Until(deadline)):
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}
}

func openURL(url string) tea.Cmd {
	return func() tea.Msg {
		if err := runCmd("open", url); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

// tailWriter keeps the last max lines written to it.
type tailWriter struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	text := t.partial + string(p)
	parts := strings.Split(text, "\n")
	t.partial = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		if line = strings.TrimSpace(line); line != "" {
			t.lines = append(t.lines, line)
		}
	}
	if len(t.lines) > t.max {
		t.lines = slices.Clone(t.lines[len(t.lines)-t.max:])
	}
	return len(p), nil
}

// last is the last non-empty line written, including an unfinished one.
func (t *tailWriter) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := strings.TrimSpace(t.partial); p != "" {
		return p
	}
	if len(t.lines) == 0 {
		return ""
	}
	return t.lines[len(t.lines)-1]
}
