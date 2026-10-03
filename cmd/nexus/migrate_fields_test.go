package main

import (
	"strings"
	"testing"
)

func TestMigrateFieldsRenamesGin(t *testing.T) {
	src := []byte(`package edge

import "github.com/paulmanoni/nexus/middleware"

func Compress() middleware.Middleware {
	return middleware.Middleware{Name: "gzip", Gin: handler}
}

type other struct{ Gin int }

var o = other{Gin: 1}
`)
	out, changes, err := migrateGoFields("edge.go", src)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `middleware.Middleware{Name: "gzip", HTTP: handler}`) || !strings.Contains(got, "other{Gin: 1}") || len(changes) != 1 {
		t.Fatalf("changes %v:\n%s", changes, got)
	}
}
