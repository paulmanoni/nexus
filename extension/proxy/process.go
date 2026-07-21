package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Command describes how to launch and supervise the upstream process (e.g. the
// legacy Django app) alongside nexus, with distinct dev/prod invocations. Under
// `nexus dev` (NEXUS_DEV=1) the Dev argv runs; a production binary runs Prod.
// Either may be empty to mean "don't launch it in this mode" — leave Prod empty
// when the legacy app is managed by systemd/k8s in production and only sidecar
// it during development.
//
//	proxy.Module(proxy.Config{
//	    Upstream: "http://127.0.0.1:8000",
//	    Command: &proxy.Command{
//	        Dev:   []string{"python", "manage.py", "runserver", "127.0.0.1:8000"},
//	        Prod:  nil, // managed externally in prod
//	        Dir:   "../legacy",
//	        Name:  "django",
//	        Ready: proxy.Ready{Path: "/healthz"},
//	    },
//	    Routes: []proxy.Route{ ... },
//	})
type Command struct {
	Dev  []string // argv for dev (NEXUS_DEV=1). Empty → not launched in dev.
	Prod []string // argv for prod. Empty → not launched (externally managed).
	Dir  string   // working directory for the child
	Env  []string // extra env vars, appended to os.Environ()
	Name string   // log-line prefix + dashboard label; default "upstream"

	// StopGrace is how long to wait after an interrupt signal before the child
	// is force-killed on shutdown. Default 5s.
	StopGrace time.Duration

	// Ready optionally gates boot on the upstream answering. Off when zero.
	Ready Ready
}

// Ready blocks boot until the upstream answers, so nexus doesn't start
// forwarding to a process that isn't listening yet. Best-effort: a timeout logs
// a warning and continues — it never aborts boot.
type Ready struct {
	Path    string        // GET <upstream><Path>; ANY HTTP response counts as up
	Timeout time.Duration // default 30s
}

func (c *Command) argsFor(dev bool) []string {
	if dev {
		return c.Dev
	}
	return c.Prod
}

// process is a supervised child. cancel triggers a graceful stop (interrupt,
// then force-kill after grace via exec.Cmd.WaitDelay).
type process struct {
	name   string
	cmd    *exec.Cmd
	cancel context.CancelFunc
	grace  time.Duration
	done   chan struct{}
}

// startProcess launches the command for the given mode. Returns (nil, nil) when
// the mode's argv is empty — a legitimate "nothing to run here" (e.g. prod).
func startProcess(c *Command, dev bool) (*process, error) {
	args := c.argsFor(dev)
	if len(args) == 0 {
		return nil, nil
	}
	name := c.Name
	if name == "" {
		name = "upstream"
	}
	grace := c.StopGrace
	if grace <= 0 {
		grace = 5 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	// Graceful stop: on ctx cancel send an interrupt (SIGINT on unix — Django's
	// runserver shuts down cleanly on it); WaitDelay force-kills if it lingers.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = grace
	prefix := []byte("[" + name + "] ")
	cmd.Stdout = &prefixWriter{w: os.Stdout, prefix: prefix}
	cmd.Stderr = &prefixWriter{w: os.Stderr, prefix: prefix}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("proxy: launch %s (%v): %w", name, args, err)
	}
	p := &process{name: name, cmd: cmd, cancel: cancel, grace: grace, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(p.done) }()
	return p, nil
}

// stop signals the child and waits for it to exit (bounded by grace).
func (p *process) stop() {
	if p == nil {
		return
	}
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(p.grace + time.Second):
	}
}

// waitReady polls the upstream until it answers or the timeout elapses. Any
// HTTP response (even 404) means the server is listening. Returns true on ready.
func waitReady(upstream, path string, timeout time.Duration) bool {
	if path == "" {
		return true
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	url := strings.TrimRight(upstream, "/") + path
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx // short dev-boot readiness poll
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

// prefixWriter prefixes each complete line written to it, so a child's stdout /
// stderr is attributable in the console (e.g. "[django] ..."). Partial trailing
// lines are buffered until their newline arrives.
type prefixWriter struct {
	w      io.Writer
	prefix []byte
	mu     sync.Mutex
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}
		line := p.buf[:i+1]
		out := make([]byte, 0, len(p.prefix)+len(line))
		out = append(out, p.prefix...)
		out = append(out, line...)
		if _, err := p.w.Write(out); err != nil {
			return len(b), err
		}
		p.buf = append([]byte(nil), p.buf[i+1:]...)
	}
	return len(b), nil
}

func modeLabel(dev bool) string {
	if dev {
		return "dev"
	}
	return "prod"
}
