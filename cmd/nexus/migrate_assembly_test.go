package main

import (
	"strings"
	"testing"
)

func TestMigrateAssemblyFlags(t *testing.T) {
	src := []byte(`package main

func main() {
	cfg.Middleware.Global = append(cfg.Middleware.Global, mw...)
	opts = append(opts, nexus.Invoke(resources.EnsureIndexes))
}
`)
	out, changes, err := migrateGoAssembly("main.go", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || !strings.Contains(string(out), "nexus.Middleware(...)") || !strings.Contains(string(out), "nexus.Setup(...)") {
		t.Fatalf("changes = %v\n%s", changes, out)
	}
	again, more, _ := migrateGoAssembly("main.go", out)
	if len(more) != 0 || string(again) != string(out) {
		t.Fatalf("second run changed the file: %v", more)
	}
}
