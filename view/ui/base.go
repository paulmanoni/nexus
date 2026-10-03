package ui

import (
	"strings"

	"github.com/a-h/templ"
)

// Variant is a component's look: a button's, a badge's, a toast's.
type Variant string

const (
	Primary   Variant = "primary"
	Secondary Variant = "secondary"
	Outline   Variant = "outline"
	Ghost     Variant = "ghost"
	Danger    Variant = "danger"
	Success   Variant = "success"
	Warning   Variant = "warning"
	Info      Variant = "info"
	LinkStyle Variant = "link" // a button that looks like a link
)

// Size is a button's size.
type Size string

const (
	Sm Size = "sm"
	Md Size = "md"
	Lg Size = "lg"
)

func join(classes ...string) string {
	var b strings.Builder
	for _, c := range classes {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(c)
	}
	return b.String()
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func merge(attrs ...templ.Attributes) templ.Attributes {
	out := templ.Attributes{}
	for _, a := range attrs {
		for k, v := range a {
			out[k] = v
		}
	}
	return out
}

func hasScript(s templ.ComponentScript) bool { return s.Call != "" }
