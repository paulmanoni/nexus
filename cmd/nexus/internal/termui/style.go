package termui

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Style colours and frames text: a 256-colour foreground, bold, and an
// optional rounded border with horizontal padding, at a fixed size.
type Style struct {
	fg, borderFg  int
	bold, border  bool
	pad           int
	width, height int
}

func NewStyle() Style { return Style{fg: -1, borderFg: -1} }

func (s Style) Foreground(color int) Style { s.fg = color; return s }
func (s Style) Bold(on bool) Style         { s.bold = on; return s }

// Border frames the text in a rounded border of color.
func (s Style) Border(color int) Style { s.border, s.borderFg = true, color; return s }

// Padding is the space between a border and the text, left and right.
func (s Style) Padding(x int) Style { s.pad = x; return s }

// Width is the whole width, border included; text is cut to fit.
func (s Style) Width(w int) Style { s.width = w; return s }

// Height is the whole height, border included; text is cut or padded.
func (s Style) Height(h int) Style { s.height = h; return s }

func (s Style) color(text string, fg int) string {
	if text == "" || (fg < 0 && !s.bold) {
		return text
	}
	var codes []string
	if s.bold {
		codes = append(codes, "1")
	}
	if fg >= 0 {
		codes = append(codes, fmt.Sprintf("38;5;%d", fg))
	}
	return "\x1b[" + strings.Join(codes, ";") + "m" + text + "\x1b[0m"
}

// Render styles text.
func (s Style) Render(text string) string {
	if !s.border {
		return s.color(text, s.fg)
	}
	lines := strings.Split(text, "\n")
	inner := 0
	if s.width > 0 {
		inner = max(s.width-2-2*s.pad, 1)
	} else {
		for _, l := range lines {
			inner = max(inner, Width(l))
		}
	}
	if s.height > 0 {
		rows := max(s.height-2, 0)
		if len(lines) > rows {
			lines = lines[len(lines)-rows:]
		}
		for len(lines) < rows {
			lines = append(lines, "")
		}
	}
	edge := Style{fg: s.borderFg}
	pad := strings.Repeat(" ", s.pad)
	bar := strings.Repeat("─", inner+2*s.pad)
	var b strings.Builder
	b.WriteString(edge.color("╭"+bar+"╮", s.borderFg))
	for _, l := range lines {
		l = Truncate(l, inner)
		b.WriteString("\n" + edge.color("│", s.borderFg) + pad + s.color(l, s.fg) + strings.Repeat(" ", inner-Width(l)) + pad + edge.color("│", s.borderFg))
	}
	b.WriteString("\n" + edge.color("╰"+bar+"╯", s.borderFg))
	return b.String()
}

// Width is how many columns s takes on screen: its characters, without
// its colour codes.
func Width(s string) int { return utf8.RuneCountInString(stripANSI(s)) }

// Height is how many lines s has.
func Height(s string) int { return strings.Count(s, "\n") + 1 }

// Truncate cuts s to w columns, keeping its colour codes whole.
func Truncate(s string, w int) string {
	if Width(s) <= w {
		return s
	}
	var b strings.Builder
	cols := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := escapeEnd(s, i)
			b.WriteString(s[i:j])
			i = j
			continue
		}
		if cols == w {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		cols++
		i += n
	}
	return b.String() + "\x1b[0m"
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = escapeEnd(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// escapeEnd is where the escape sequence at s[i] ends.
func escapeEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		for j++; j < len(s) && (s[j] < 0x40 || s[j] > 0x7e); j++ {
		}
		return min(j+1, len(s))
	}
	return min(j+1, len(s))
}
