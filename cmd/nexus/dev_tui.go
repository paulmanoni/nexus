package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runDevTUI is the bubble-tea-backed alternative to runDev. Same
// process-management contract (build child, kill on Ctrl-C, port
// probe), but streams output into a fixed-layout terminal UI:
// header on top with the dashboard URL + ready state, log pane
// below with the child's stdout/stderr.
//
// Hotkeys:
//   q, ctrl-c   quit (kills the child group)
//   r           restart the child (rebuild + spawn)
//   o           open the dashboard URL in the default browser
//   c           clear the log pane
//
// Picking a TUI mode is deliberately opt-in (--tui) — a TUI takes
// over the whole terminal and is unfriendly when piping (`nexus dev
// | grep`); the static-banner mode stays the default.
func runDevTUI(target, addr string, stdout, stderr io.Writer) error {
	model := newTUIModel(target, addr)
	prog := tea.NewProgram(model, tea.WithAltScreen())
	model.prog = prog

	// Kick off the first child process. Errors here surface in the
	// TUI's log pane via logLineMsg; we don't return early.
	go model.spawnChild()

	// Background port probe — when it succeeds the TUI flips to the
	// "ready" state. Bounded by 60s so a broken app doesn't loop
	// forever; the user can still hit `r` to retry.
	go model.watchReady()

	if _, err := prog.Run(); err != nil {
		return err
	}
	model.killChild()
	_ = stdout
	_ = stderr // taken over by the TUI; the args are kept for parity with runDev
	return nil
}

// --- Model ---

type tuiModel struct {
	target string
	addr   string
	url    string

	width, height int
	state         tuiState
	stateMsg      string

	logs    []string // ring buffer of recent log lines
	logsMu  sync.Mutex
	maxLogs int

	childMu  sync.Mutex
	child    *exec.Cmd
	childGen int // increments on every restart so stale goroutines bail out

	prog *tea.Program // back-reference for goroutines that need to .Send()
}

type tuiState int

const (
	tuiStarting tuiState = iota
	tuiBuilding
	tuiReady
	tuiCrashed
	tuiRestarting
)

func newTUIModel(target, addr string) *tuiModel {
	return &tuiModel{
		target:  target,
		addr:    addr,
		url:     dashboardURL(addr),
		state:   tuiStarting,
		maxLogs: 2000,
	}
}

// --- Tea messages ---

type logLineMsg string
type stateChangeMsg struct {
	state tuiState
	note  string
}
type tickMsg time.Time

// --- Init / Update / View ---

func (m *tuiModel) Init() tea.Cmd {
	return tickEvery(500 * time.Millisecond)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "r":
			m.appendLog("[nexus dev] restarting…")
			m.state = tuiRestarting
			go func() {
				m.killChild()
				m.spawnChild()
			}()
			return m, nil
		case "o":
			_ = openBrowser(m.url)
			return m, nil
		case "c":
			m.logsMu.Lock()
			m.logs = m.logs[:0]
			m.logsMu.Unlock()
			return m, nil
		}

	case logLineMsg:
		m.appendLog(string(msg))
		return m, nil

	case stateChangeMsg:
		m.state = msg.state
		m.stateMsg = msg.note
		return m, nil

	case tickMsg:
		// Drive periodic re-renders so timestamps / ready probes
		// surface without waiting for the next log line.
		return m, tickEvery(500 * time.Millisecond)
	}
	return m, nil
}

func (m *tuiModel) View() string {
	if m.width == 0 {
		return "" // size not yet known
	}
	header := m.renderHeader()
	footer := m.renderFooter()

	// Reserve fixed rows for header (3) + spacer (1) + footer (1) =
	// 5; the rest goes to the log pane. Cap at width so logs don't
	// shred-wrap on narrow terminals.
	logHeight := m.height - 5
	if logHeight < 3 {
		logHeight = 3
	}
	logs := m.renderLogs(logHeight)

	return lipgloss.JoinVertical(lipgloss.Left,
		header,
		"",
		logs,
		footer,
	)
}

// --- Renderers ---

var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleCyan   = lipgloss.NewStyle().Foreground(lipgloss.Color("36"))
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	stylePane   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("241")).Padding(0, 1)
)

func (m *tuiModel) renderHeader() string {
	state := m.renderState()
	left := styleTitle.Render("nexus dev")
	mid := styleDim.Render("· " + m.target)
	right := styleCyan.Render(m.url)
	row1 := strings.Join([]string{left, mid, right}, "  ")
	row2 := state
	return lipgloss.JoinVertical(lipgloss.Left, row1, row2)
}

func (m *tuiModel) renderState() string {
	switch m.state {
	case tuiStarting:
		return styleYellow.Render("● starting") + styleDim.Render(" "+m.stateMsg)
	case tuiBuilding:
		return styleYellow.Render("● building") + styleDim.Render(" "+m.stateMsg)
	case tuiReady:
		return styleGreen.Render("● ready")
	case tuiCrashed:
		return styleRed.Render("● crashed") + styleDim.Render(" "+m.stateMsg)
	case tuiRestarting:
		return styleYellow.Render("● restarting")
	}
	return ""
}

func (m *tuiModel) renderLogs(height int) string {
	m.logsMu.Lock()
	defer m.logsMu.Unlock()

	// Show the last `height-2` lines (account for pane borders).
	rows := height - 2
	if rows < 1 {
		rows = 1
	}
	start := len(m.logs) - rows
	if start < 0 {
		start = 0
	}
	visible := m.logs[start:]

	// Truncate each line to the inner width. lipgloss handles
	// terminal wrapping but truncation keeps the layout deterministic
	// — child output occasionally has ANSI we can't safely re-wrap.
	innerW := m.width - 4
	if innerW < 10 {
		innerW = 10
	}
	clipped := make([]string, 0, len(visible))
	for _, l := range visible {
		if lipgloss.Width(l) > innerW {
			clipped = append(clipped, l[:innerW])
		} else {
			clipped = append(clipped, l)
		}
	}
	body := strings.Join(clipped, "\n")
	return stylePane.Width(m.width - 2).Height(height).Render(body)
}

func (m *tuiModel) renderFooter() string {
	keys := strings.Join([]string{
		styleDim.Render("[q]") + " quit",
		styleDim.Render("[r]") + " restart",
		styleDim.Render("[o]") + " open",
		styleDim.Render("[c]") + " clear",
	}, "  ")
	return styleDim.Render(keys)
}

// --- Helpers ---

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *tuiModel) appendLog(line string) {
	m.logsMu.Lock()
	defer m.logsMu.Unlock()
	m.logs = append(m.logs, strings.TrimRight(line, "\r\n"))
	if len(m.logs) > m.maxLogs {
		m.logs = m.logs[len(m.logs)-m.maxLogs:]
	}
}

// spawnChild starts a fresh `go run` of the target. Stdout + stderr
// are piped through io.Pipe → bufio.Scanner → tea.Program.Send, so
// every line of child output becomes a logLineMsg in the model's
// Update loop.
func (m *tuiModel) spawnChild() {
	m.childMu.Lock()
	gen := m.childGen + 1
	m.childGen = gen
	m.childMu.Unlock()

	m.send(stateChangeMsg{state: tuiStarting, note: "go run " + m.target})

	cmd := exec.Command("go", "run", m.target)
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.Stdin = nil
	setProcessGroup(cmd)

	go m.streamLines(stdoutR, gen)
	go m.streamLines(stderrR, gen)

	if err := cmd.Start(); err != nil {
		m.send(logLineMsg("[nexus dev] start failed: " + err.Error()))
		m.send(stateChangeMsg{state: tuiCrashed, note: err.Error()})
		_ = stdoutW.Close()
		_ = stderrW.Close()
		return
	}

	m.childMu.Lock()
	m.child = cmd
	m.childMu.Unlock()

	// Wait in this goroutine so a crash flips the state without
	// needing a separate watcher.
	err := cmd.Wait()
	_ = stdoutW.Close()
	_ = stderrW.Close()

	m.childMu.Lock()
	currentGen := m.childGen
	m.childMu.Unlock()
	if currentGen != gen {
		// A restart already replaced this child; don't stomp the new
		// state.
		return
	}
	if err != nil {
		m.send(stateChangeMsg{state: tuiCrashed, note: err.Error()})
	} else {
		m.send(stateChangeMsg{state: tuiCrashed, note: "exited cleanly"})
	}
}

// killChild sends SIGTERM to the entire process group, then SIGKILL
// after a grace period. Identical contract to runDev's kill path so
// no stale binaries hold the port across restarts.
func (m *tuiModel) killChild() {
	m.childMu.Lock()
	cmd := m.child
	m.child = nil
	m.childMu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = killProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
	deadline := time.After(5 * time.Second)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-deadline:
		_ = killProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// streamLines reads child output line-by-line and forwards each one
// as a logLineMsg. The gen check guards against late writes from a
// prior restart's pipes leaking into the new run's view.
func (m *tuiModel) streamLines(r io.Reader, gen int) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		m.childMu.Lock()
		current := m.childGen
		m.childMu.Unlock()
		if current != gen {
			return
		}
		m.send(logLineMsg(scanner.Text()))
	}
}

// watchReady polls the listen address until it accepts a connection,
// then flips state to ready. Bounded by 60s — after that the user
// owes a restart (`r`) for the probe to retry.
func (m *tuiModel) watchReady() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	probe := normalizeProbeAddr(m.addr)
	for ctx.Err() == nil {
		conn, err := net.DialTimeout("tcp", probe, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			m.send(stateChangeMsg{state: tuiReady})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *tuiModel) send(msg tea.Msg) {
	if m.prog == nil {
		return
	}
	m.prog.Send(msg)
}

// silence the os import unused-warning on platforms where it's not
// otherwise referenced through the build path above.
var _ = os.Stdout