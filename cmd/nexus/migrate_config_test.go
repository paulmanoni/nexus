package main

import (
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
)

func TestMigrateNexusTOML_MovesMisplacedKeys(t *testing.T) {
	src := `# app config
environment = "production"   # was ignored by v1
introspection = true

[runtime]
addr = ":9000"
version = "1.2.3"

[runtime.server]
route_prefix = "/api"

[runtime.dashboard]
name = "Shop"
curency = "EUR"
`
	out, changes, err := migrateNexusTOMLKeys("nexus.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := `# app config

[runtime]
version = "1.2.3"
environment = "production"   # was ignored by v1
introspection = true

[runtime.server]
route_prefix = "/api"
addr = ":9000"

[runtime.dashboard]
name = "Shop"
curency = "EUR"
`
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if len(changes) != 3 || changes[0].Line != 2 || !strings.HasPrefix(changes[0].New, "[runtime] environment") {
		t.Errorf("changes = %+v", changes)
	}
	// The typo is not a move; the check still reports it.
	problems, _ := config.Check(out, "nexus.toml")
	if len(problems) != 1 || problems[0].Key() != "runtime.dashboard.curency" {
		t.Errorf("after migrate: %+v", problems)
	}
	// Idempotent.
	again, ch, err := migrateNexusTOMLKeys("nexus.toml", out)
	if err != nil || len(ch) != 0 || string(again) != string(out) {
		t.Errorf("second run changed the file: %v %+v", err, ch)
	}
}

func TestMigrateNexusTOML_NewTableAndConflicts(t *testing.T) {
	src := `trace_capacity = 50
sdk = [
  true,
]

[runtime]
sdk = false

[runtime.server]
addr = ":1"

[runtime.dashboard]
addr = ":2"
`
	out, changes, err := migrateNexusTOMLKeys("nexus.toml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// trace_capacity moves into the existing [runtime]; the multi-line sdk
	// stays; [runtime.dashboard] addr stays because [runtime.server] sets one.
	if len(changes) != 1 || !strings.Contains(got, "[runtime]\nsdk = false\ntrace_capacity = 50\n") {
		t.Errorf("changes=%+v\n%s", changes, got)
	}
	if !strings.Contains(got, "[runtime.dashboard]\naddr = \":2\"") {
		t.Errorf("conflicting key moved:\n%s", got)
	}

	out, changes, err = migrateNexusTOMLKeys("nexus.toml", []byte("[runtime]\nroute_prefix = \"/v1\"\n"))
	if err != nil || len(changes) != 1 {
		t.Fatalf("err=%v changes=%+v", err, changes)
	}
	if want := "[runtime]\n\n[runtime.server]\nroute_prefix = \"/v1\"\n"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}
