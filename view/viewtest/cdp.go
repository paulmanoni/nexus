package viewtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// A small Chrome DevTools Protocol client: enough to launch a headless
// Chrome, open one tab and drive it. No third-party CDP library — the
// protocol is JSON over the WebSocket nexus already links.

// findChrome returns a Chrome or Chromium binary: $NEXUS_CHROME, then the
// usual install locations and PATH names; "" when there is none.
func findChrome() string {
	if p := os.Getenv("NEXUS_CHROME"); p != "" {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// chrome is a running headless Chrome and the DevTools connection to it.
type chrome struct {
	cmd  *exec.Cmd
	dir  string
	conn *websocket.Conn

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpMessage
	events  []func(cdpMessage)
	closed  bool
}

type cdpMessage struct {
	ID        int             `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// launchChrome starts Chrome headless with a throwaway profile and connects
// to its browser-level DevTools endpoint.
func launchChrome(bin string, width, height int) (*chrome, error) {
	dir, err := os.MkdirTemp("", "nexus-viewtest-chrome-")
	if err != nil {
		return nil, err
	}
	args := []string{
		"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-background-networking", "--mute-audio",
		"--remote-debugging-port=0", "--user-data-dir=" + dir,
		fmt.Sprintf("--window-size=%d,%d", width, height), "about:blank",
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		args = append(args, "--no-sandbox") // Chrome refuses root with the sandbox (CI containers)
	}
	cmd := exec.Command(bin, args...)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c := &chrome{cmd: cmd, dir: dir, pending: map[int]chan cdpMessage{}}
	wsURL, err := devToolsURL(dir, 15*time.Second)
	if err != nil {
		c.close()
		return nil, err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		c.close()
		return nil, fmt.Errorf("connecting to Chrome DevTools: %w", err)
	}
	conn.SetReadLimit(64 << 20) // screenshots
	c.conn = conn
	go c.read()
	return c, nil
}

// devToolsURL waits for Chrome to write DevToolsActivePort (port, then the
// browser endpoint's path) into its profile.
func devToolsURL(dir string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := os.Open(filepath.Join(dir, "DevToolsActivePort"))
		if err == nil {
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			f.Close()
			if len(lines) >= 2 && lines[0] != "" && lines[1] != "" {
				return "ws://127.0.0.1:" + lines[0] + lines[1], nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", errors.New("Chrome did not open its DevTools port")
}

func (c *chrome) read() {
	for {
		var m cdpMessage
		if err := c.conn.ReadJSON(&m); err != nil {
			c.mu.Lock()
			c.closed = true
			for id, ch := range c.pending {
				close(ch)
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		if m.ID != 0 {
			if ch, ok := c.pending[m.ID]; ok {
				delete(c.pending, m.ID)
				ch <- m
			}
		} else {
			for _, fn := range c.events {
				fn(m)
			}
		}
		c.mu.Unlock()
	}
}

// call sends method to the browser (session "") or a tab's session and
// decodes its result into out (nil to discard).
func (c *chrome) call(ctx context.Context, session, method string, params, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("Chrome is closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan cdpMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	c.writeMu.Lock()
	err := c.conn.WriteJSON(msg)
	c.writeMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("Chrome closed the connection")
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// on registers fn for every event; it runs on the reader goroutine.
func (c *chrome) on(fn func(cdpMessage)) {
	c.mu.Lock()
	c.events = append(c.events, fn)
	c.mu.Unlock()
}

func (c *chrome) close() {
	if c.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.call(ctx, "", "Browser.close", nil, nil)
		cancel()
		_ = c.conn.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		}
	}
	_ = os.RemoveAll(c.dir)
}
