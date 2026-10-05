package termui

import (
	"strings"
	"testing"
)

func TestKeys(t *testing.T) {
	got := strings.Join(keys([]byte("q\x03r\x1b[A\x1bé")), ",")
	if got != "q,ctrl+c,r,esc,é" {
		t.Errorf("keys = %q", got)
	}
}

func TestWidthAndTruncate(t *testing.T) {
	s := NewStyle().Foreground(203).Render("héllo") + " world"
	if Width(s) != 11 {
		t.Errorf("Width = %d", Width(s))
	}
	cut := Truncate(s, 3)
	if Width(cut) != 3 || !strings.Contains(cut, "\x1b[38;5;203m") {
		t.Errorf("Truncate = %q", cut)
	}
}

func TestBorder(t *testing.T) {
	box := NewStyle().Border(241).Padding(1).Width(12).Height(4).Render("a\nb\nc")
	lines := strings.Split(box, "\n")
	if len(lines) != 4 {
		t.Fatalf("%d lines:\n%s", len(lines), box)
	}
	for _, l := range lines {
		if Width(l) != 12 {
			t.Errorf("line %q is %d wide", stripANSI(l), Width(l))
		}
	}
	if !strings.Contains(stripANSI(lines[2]), "c") || strings.Contains(box, "a") {
		t.Errorf("a box of 2 rows keeps the last lines:\n%s", stripANSI(box))
	}
}
