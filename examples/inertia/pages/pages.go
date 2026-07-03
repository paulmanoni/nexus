// Package pages holds the Inertia page handlers, registered via the
// //@inertia.Page custom decorator — the codegen emits
// decorate.Record(inertia.Page("GET", "/users", "Users/Index", NewUsers)),
// reusing the extension's existing Option-returning registrar with no wrapper.
package pages

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

type usersProps struct {
	Users []string `json:"users"`
}

// NewUsers renders the "Users/Index" component. Its return value is the page's
// props (rendered through the Inertia protocol), not a JSON body.
//
// @inertia.Page "GET" "/users" "Users/Index"
func NewUsers(p nexus.Params[struct{}]) (usersProps, error) {
	return usersProps{Users: []string{"alice", "bob"}}, nil
}

type loginProps struct {
	Title string `json:"title"`
}

// NewLogin renders on GET and redirects on POST. It uses inertia.Redirect,
// which is also why this file imports inertia — so the //@inertia.Page selector
// resolves to the inertia import in the generated file.
//
// @inertia.Page "GET,POST" "/login" "Auth/Login"
func NewLogin(p nexus.Params[struct{}]) (any, error) {
	if p.Method == "POST" {
		return nil, inertia.Redirect("/users")
	}
	return loginProps{Title: "Sign in"}, nil
}
