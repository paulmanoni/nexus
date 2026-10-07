package ui

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// The kit's sources, for `nexus add ui`: an app that wants to own a
// component gets a copy of its template and of what it needs.
//
//go:embed *.templ base.go commands.go ui.go ui.js ui.css
var sources embed.FS

// core is what every vendored component needs: the helpers, the JS
// commands dialogs and tabs run, the assets and Script/Loading, and the
// icons.
var core = []string{"base.go", "commands.go", "ui.go", "ui.js", "ui.css", "icon.templ"}

// components maps a component name to its template and the components it
// renders.
var components = map[string]struct {
	file  string
	needs []string
}{
	"button": {"button.templ", nil},
	"field":  {"field.templ", nil},
	"forms":  {"formfields.templ", []string{"field"}},
	"tabs":   {"tabs.templ", nil},
	"dialog": {"dialog.templ", nil},
	"menu":   {"menu.templ", []string{"button"}},
	"table":  {"table.templ", []string{"menu", "field"}},
	"toast":  {"toast.templ", nil},
	"loader": {"loader.templ", nil},
	"header": {"header.templ", nil},
	"badge":  {"badge.templ", nil},
	"icon":   {"icon.templ", nil},
}

// aliases are the component (function) names that live in another file.
var aliases = map[string]string{
	"input": "field", "select": "field", "textarea": "field", "checkbox": "field",
	"tabpanel": "tabs", "dropdown": "menu", "menubutton": "menu", "rowactions": "menu",
	"datatable": "table", "tablerow": "table", "emptystate": "table", "searchinput": "table",
	"skeleton": "loader", "pageheader": "header",
	"textfield": "forms", "textareafield": "forms", "selectfield": "forms", "checkboxfield": "forms",
	"switchfield": "forms", "submitbutton": "forms",
}

// ComponentNames lists what Vendor accepts (aliases aside), sorted.
func ComponentNames() []string {
	names := make([]string, 0, len(components))
	for n := range components {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Vendor returns the files that make up the named components — "button",
// "table", "all", or a component function's name ("DataTable") — and what
// they need, file name → content, as the kit has them (package ui).
func Vendor(names ...string) (map[string][]byte, error) {
	want := map[string]bool{}
	var add func(string) error
	add = func(name string) error {
		key := strings.ToLower(strings.TrimSpace(name))
		if a, ok := aliases[key]; ok {
			key = a
		}
		c, ok := components[key]
		if !ok {
			return fmt.Errorf("no ui component %q (have: %s, all)", name, strings.Join(ComponentNames(), ", "))
		}
		if want[c.file] {
			return nil
		}
		want[c.file] = true
		for _, n := range c.needs {
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range names {
		if strings.EqualFold(n, "all") {
			for _, c := range ComponentNames() {
				if err := add(c); err != nil {
					return nil, err
				}
			}
			continue
		}
		if err := add(n); err != nil {
			return nil, err
		}
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("name a ui component (have: %s, all)", strings.Join(ComponentNames(), ", "))
	}
	for _, f := range core {
		want[f] = true
	}
	out := make(map[string][]byte, len(want))
	for f := range want {
		b, err := sources.ReadFile(f)
		if err != nil {
			return nil, err
		}
		out[f] = b
	}
	return out, nil
}
