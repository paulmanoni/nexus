// Package termui is the small full-screen terminal UI nexus dev --tui
// draws: an event loop over keys, window sizes and the program's own
// messages, a model that renders the whole screen, and 256-colour styles.
// It replaces a TUI framework the CLI needed only this much of.
package termui

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Msg is anything the model updates on.
type Msg any

// Cmd produces a message later, run on a goroutine of its own.
type Cmd func() Msg

// Model is the screen's state: Update takes a message, View draws it all.
type Model interface {
	Init() Cmd
	Update(Msg) (Model, Cmd)
	View() string
}

// KeyMsg is a key the user pressed: "q", "r", "ctrl+c", …
type KeyMsg string

func (k KeyMsg) String() string { return string(k) }

// WindowSizeMsg is the terminal's size, sent first and on every change.
type WindowSizeMsg struct{ Width, Height int }

type quitMsg struct{}

// Quit is the Cmd that ends the program.
func Quit() Msg { return quitMsg{} }

// Tick is a Cmd that sends fn's message after d.
func Tick(d time.Duration, fn func(time.Time) Msg) Cmd {
	return func() Msg { return fn(<-time.After(d)) }
}

// Program runs a model on the terminal's alternate screen.
type Program struct {
	model Model
	msgs  chan Msg
	in    *os.File
	out   io.Writer
	once  sync.Once
	done  chan struct{}
}

func NewProgram(m Model) *Program {
	return &Program{model: m, msgs: make(chan Msg, 64), in: os.Stdin, out: os.Stdout, done: make(chan struct{})}
}

// Send gives the model msg; it waits while the program is busy, and does
// nothing once it has ended.
func (p *Program) Send(msg Msg) {
	select {
	case p.msgs <- msg:
	case <-p.done:
	}
}

// Quit ends the program, from any goroutine.
func (p *Program) Quit() { p.Send(quitMsg{}) }

// Run takes over the terminal until the model quits, and gives it back as
// it was.
func (p *Program) Run() (Model, error) {
	fd := int(p.in.Fd())
	if !term.IsTerminal(fd) {
		return p.model, errors.New("termui: stdin is not a terminal")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return p.model, err
	}
	defer func() { _ = term.Restore(fd, state) }()
	io.WriteString(p.out, "\x1b[?1049h\x1b[?25l") // alternate screen, no cursor
	defer io.WriteString(p.out, "\x1b[?25h\x1b[?1049l")
	defer p.once.Do(func() { close(p.done) })

	go p.readKeys()
	go p.watchSize(fd)
	p.run(p.model.Init())
	for msg := range p.msgs {
		if _, ok := msg.(quitMsg); ok {
			return p.model, nil
		}
		var cmd Cmd
		p.model, cmd = p.model.Update(msg)
		p.run(cmd)
		p.draw()
	}
	return p.model, nil
}

func (p *Program) run(cmd Cmd) {
	if cmd != nil {
		go func() { p.Send(cmd()) }()
	}
}

// draw writes the whole view from the top-left corner, clearing what each
// line and the screen below it held before.
func (p *Program) draw() {
	lines := strings.Split(p.model.View(), "\n")
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l)
		b.WriteString("\x1b[0m\x1b[K")
	}
	b.WriteString("\x1b[J")
	io.WriteString(p.out, b.String())
}

func (p *Program) readKeys() {
	buf := make([]byte, 64)
	for {
		n, err := p.in.Read(buf)
		if err != nil {
			return
		}
		for _, k := range keys(buf[:n]) {
			p.Send(KeyMsg(k))
		}
	}
}

// keys names the keys in a read from the terminal: ctrl+letter, escape,
// printable characters; escape sequences (arrows) are dropped.
func keys(b []byte) []string {
	var out []string
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == 0x1b:
			if i+1 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
				for i += 2; i < len(b) && (b[i] < 0x40 || b[i] > 0x7e); i++ {
				}
				continue
			}
			out = append(out, "esc")
		case c == '\r' || c == '\n':
			out = append(out, "enter")
		case c < 0x20:
			out = append(out, "ctrl+"+string(rune('a'+c-1)))
		case c == 0x7f:
			out = append(out, "backspace")
		default:
			r := []rune(string(b[i:]))[0]
			out = append(out, string(r))
			i += len(string(r)) - 1
		}
	}
	return out
}

func (p *Program) watchSize(fd int) {
	w, h := -1, -1
	for {
		nw, nh, err := term.GetSize(fd)
		if err != nil || nw == 0 || nh == 0 {
			nw, nh = 80, 24 // a terminal that doesn't say: the classic size
		}
		if nw != w || nh != h {
			w, h = nw, nh
			p.Send(WindowSizeMsg{Width: w, Height: h})
		}
		select {
		case <-p.done:
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}
