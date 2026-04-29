package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
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
func runDevTUI(target, addr string, openDash bool, stdout, stderr io.Writer) error {
	model := newTUIModel(target, addr, openDash)
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
	target   string
	addr     string // --addr flag value; the initial probe target
	bindURL  string // resolved HTTP base, populated once we detect "nexus: listening on…"
	openDash bool   // when true, 'o' / header URL points at /__nexus/ instead of /

	width, height int
	state         tuiState
	stateMsg      string

	logs    []string // ring buffer of recent log lines
	logsMu  sync.Mutex
	maxLogs int

	stats     []endpointStat
	statsMu   sync.Mutex
	statsErr  string // last poll error, displayed dim if set

	childMu  sync.Mutex
	child    *exec.Cmd
	childGen int // increments on every restart so stale goroutines bail out

	prog *tea.Program // back-reference for goroutines that need to .Send()
}

// endpointStat is the trimmed shape of metrics.EndpointStats we
// render in the TUI. Pulled into our own type so we don't import
// the metrics package's heavier deps for one struct.
type endpointStat struct {
	Key    string `json:"key"`
	Count  int64  `json:"count"`
	Errors int64  `json:"errors"`
}

type tuiState int

const (
	tuiStarting tuiState = iota
	tuiBuilding
	tuiReady
	tuiCrashed
	tuiRestarting
)

func newTUIModel(target, addr string, openDash bool) *tuiModel {
	return &tuiModel{
		target:   target,
		addr:     addr,
		openDash: openDash,
		state:    tuiStarting,
		maxLogs:  2000,
	}
}

// dashURL returns the URL to render in the header and target when
// the user hits 'o': the resolved bind once we've seen one, otherwise
// the --addr probe target as a best-effort guess. Defaults to the
// app's root URL; flips to /__nexus/ when --open-dash was passed.
func (m *tuiModel) dashURL() string {
	suffix := "/"
	if m.openDash {
		suffix = "/__nexus/"
	}
	if m.bindURL != "" {
		return m.bindURL + suffix
	}
	if m.openDash {
		return dashboardURL(m.addr)
	}
	return clientURL(m.addr)
}

// --- Tea messages ---

type logLineMsg string
type stateChangeMsg struct {
	state tuiState
	note  string
}
type tickMsg time.Time
type addrDetectedMsg string // bare address from "nexus: listening on …"
type statsMsg struct {
	stats []endpointStat
	err   error
}

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
			_ = openBrowser(m.dashURL())
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

	case addrDetectedMsg:
		// Framework printed "nexus: listening on <addr>". Resolve to
		// a clickable HTTP base + kick off the stats poller. We
		// don't probe the port here — readiness has already been
		// established (the framework only prints this line after
		// net.Listen succeeded).
		host := normalizeProbeAddr(string(msg))
		m.bindURL = "http://" + host
		m.state = tuiReady
		go m.pollStats()
		return m, nil

	case statsMsg:
		m.statsMu.Lock()
		if msg.err != nil {
			m.statsErr = msg.err.Error()
		} else {
			m.stats = msg.stats
			m.statsErr = ""
		}
		m.statsMu.Unlock()
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
	stats := m.renderStats()

	// Reserve rows for header (3), spacer (1), stats (variable),
	// spacer (1), footer (1). Whatever is left goes to the log
	// pane. Stats height is the lesser of (top-N+2) and the actual
	// number of lines we'd render — when there are no stats we
	// don't reserve space at all.
	statsLines := lipgloss.Height(stats)
	if stats == "" {
		statsLines = 0
	}
	overhead := 3 + 1 + statsLines + 1
	if statsLines > 0 {
		overhead++ // spacer between stats and logs
	}
	logHeight := m.height - overhead
	if logHeight < 3 {
		logHeight = 3
	}
	logs := m.renderLogs(logHeight)

	parts := []string{header, ""}
	if stats != "" {
		parts = append(parts, stats, "")
	}
	parts = append(parts, logs, footer)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
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
	right := styleCyan.Render(m.dashURL())
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

// renderStats renders the top-N busiest endpoints as a single pane.
// Sorted by request count desc; ties broken by error count desc, then
// by key for stability. Empty when no stats have arrived (avoids an
// awkward empty box during the first second of operation).
func (m *tuiModel) renderStats() string {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	if len(m.stats) == 0 {
		return ""
	}
	const topN = 5
	sorted := make([]endpointStat, len(m.stats))
	copy(sorted, m.stats)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		if sorted[i].Errors != sorted[j].Errors {
			return sorted[i].Errors > sorted[j].Errors
		}
		return sorted[i].Key < sorted[j].Key
	})
	if len(sorted) > topN {
		sorted = sorted[:topN]
	}
	rows := make([]string, 0, len(sorted))
	for _, s := range sorted {
		errPart := ""
		if s.Errors > 0 {
			errPart = "  " + styleRed.Render(fmt.Sprintf("%d err", s.Errors))
		}
		rows = append(rows, fmt.Sprintf("%-32s %s%s",
			s.Key,
			styleDim.Render(fmt.Sprintf("%d req", s.Count)),
			errPart,
		))
	}
	body := strings.Join(rows, "\n")
	return stylePane.Width(m.width - 2).Render(
		styleDim.Render("stats")+"\n"+body,
	)
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

// streamLines reads child output line-by-line, scans each line for
// the framework's "nexus: listening on …" startup announcement, and
// forwards every line as a logLineMsg. The gen check guards against
// late writes from a prior restart's pipes leaking into the new
// run's view.
func (m *tuiModel) streamLines(r io.Reader, gen int) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	addrSent := false
	for scanner.Scan() {
		m.childMu.Lock()
		current := m.childGen
		m.childMu.Unlock()
		if current != gen {
			return
		}
		line := scanner.Text()
		if !addrSent {
			if match := ginListenRE.FindStringSubmatch(line); match != nil {
				addrSent = true
				m.send(addrDetectedMsg(match[1]))
			}
		}
		m.send(logLineMsg(line))
	}
}

// pollStats fetches /__nexus/stats once a second and pushes a
// statsMsg into the program. Continues until the program ends —
// we deliberately don't keep a context here; goroutine exits
// naturally when prog.Send returns (post-Quit).
func (m *tuiModel) pollStats() {
	if m.bindURL == "" {
		return
	}
	url := m.bindURL + "/__nexus/stats"
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	for {
		stats, err := fetchStats(client, url)
		m.send(statsMsg{stats: stats, err: err})
		time.Sleep(time.Second)
	}
}

// fetchStats does the HTTP round-trip and decode for one poll. The
// server returns `{"stats": [...]}`; we descend into that wrapper.
func fetchStats(client *http.Client, url string) ([]endpointStat, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats: %s", resp.Status)
	}
	var body struct {
		Stats []endpointStat `json:"stats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Stats, nil
}

// watchReady polls the --addr fallback until it accepts a connection
// OR the addrDetectedMsg path flips state to ready first. Bounded by
// 60s — after that the user owes a restart (`r`) for the probe to
// retry. Used as a backstop for apps that don't print the
// framework's "nexus: listening on …" line (bare gin, etc.).
func (m *tuiModel) watchReady() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	probe := normalizeProbeAddr(m.addr)
	for ctx.Err() == nil {
		// If the addrDetectedMsg path already populated bindURL,
		// we're done — the dispatch in Update() already set state
		// to ready and started the stats poller.
		if m.bindURL != "" {
			return
		}
		conn, err := net.DialTimeout("tcp", probe, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			// Synthesize an addrDetectedMsg so the rest of the
			// pipeline (state flip, stats poller) runs the same
			// path as a framework-announced bind.
			m.send(addrDetectedMsg(m.addr))
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