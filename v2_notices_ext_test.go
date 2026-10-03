package nexus_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/internal/v2notice"
)

// v2Capture turns notices on (or off) for one test and captures the output.
func v2Capture(t *testing.T, dev bool) *bytes.Buffer {
	t.Helper()
	if dev {
		t.Setenv("NEXUS_DEV", "1")
	} else {
		t.Setenv("NEXUS_DEV", "")
	}
	t.Setenv("NEXUS_V2_NOTICES", "")
	var buf bytes.Buffer
	prev := v2notice.Output
	v2notice.Output = &buf
	v2notice.Reset()
	t.Cleanup(func() {
		v2notice.Output = prev
		v2notice.Reset()
	})
	return &buf
}

func v2Names() []string {
	var out []string
	for _, it := range v2notice.Items() {
		out = append(out, it.Name)
	}
	return out
}

func TestV2Notice_OffOutsideDev(t *testing.T) {
	buf := v2Capture(t, false)
	_ = nexus.Get[string]("app.name")
	_ = nexus.NewErrors()
	v2notice.Flush()
	if n := v2Names(); len(n) != 0 {
		t.Fatalf("recorded outside dev: %v", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("printed outside dev: %q", buf.String())
	}
}

func TestV2Notice_SilencedByEnv(t *testing.T) {
	v2Capture(t, true)
	t.Setenv("NEXUS_V2_NOTICES", "0")
	v2notice.Reset()
	_ = nexus.Get[string]("app.name")
	if n := v2Names(); len(n) != 0 {
		t.Fatalf("recorded with NEXUS_V2_NOTICES=0: %v", n)
	}
}

func TestV2Notice_OncePerProcessWithCallSite(t *testing.T) {
	buf := v2Capture(t, true)
	for range 3 {
		_ = nexus.Get[string]("app.name")
	}
	items := v2notice.Items()
	if len(items) != 1 || items[0].Name != "nexus.Get" {
		t.Fatalf("items = %+v, want one nexus.Get", items)
	}
	if !strings.Contains(items[0].Where, "v2_notices_ext_test.go:") {
		t.Fatalf("where = %q, want this file", items[0].Where)
	}

	v2notice.Flush()
	out := buf.String()
	if strings.Count(out, "nexus.Get") != 1 || !strings.Contains(out, "config.Get") {
		t.Fatalf("block:\n%s", out)
	}

	// After the block, a new use prints on its own, once; a repeat is silent.
	buf.Reset()
	_ = nexus.NewErrors()
	_ = nexus.NewErrors()
	if c := strings.Count(buf.String(), "nexus.NewErrors"); c != 1 {
		t.Fatalf("late notice printed %d times:\n%s", c, buf.String())
	}
	v2notice.Flush()
	if strings.Count(buf.String(), "nexus.NewErrors") != 1 {
		t.Fatalf("second Flush reprinted:\n%s", buf.String())
	}
}

func TestV2Notice_FrameworkCallsAreSilent(t *testing.T) {
	v2Capture(t, true)
	// MustLoadConfig calls LoadConfig: only the app's own call counts.
	path := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(path, []byte("[runtime]\nenvironment = \"development\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = nexus.MustLoadConfig(path)
	if n := v2Names(); len(n) != 1 || n[0] != "nexus.MustLoadConfig" {
		t.Fatalf("names = %v, want only nexus.MustLoadConfig", n)
	}
}

func NewListPets(ctx context.Context, _ struct{}) ([]string, error) { return nil, nil }
func NewCreatePet(ctx context.Context, _ struct{}) (string, error)  { return "", nil }
func ListOwners(ctx context.Context, _ struct{}) ([]string, error)  { return nil, nil }

func TestV2Notice_NewOpName(t *testing.T) {
	v2Capture(t, true)
	_ = nexus.AsQuery(NewListPets)
	_ = nexus.AsMutation(NewCreatePet, nexus.Op("createPet"))
	_ = nexus.AsQuery(ListOwners)

	items := v2notice.Items()
	if len(items) != 1 {
		t.Fatalf("items = %+v, want only NewListPets", items)
	}
	it := items[0]
	if it.Name != `op "listPets" from NewListPets` {
		t.Fatalf("name = %q", it.Name)
	}
	for _, want := range []string{`"newListPets"`, `nexus.Op("listPets")`} {
		if !strings.Contains(it.Replacement, want) {
			t.Fatalf("replacement %q lacks %s", it.Replacement, want)
		}
	}
	if !strings.Contains(it.Where, "v2_notices_ext_test.go:") {
		t.Fatalf("where = %q", it.Where)
	}
}

func TestV2Notice_RemovedAPIs(t *testing.T) {
	v2Capture(t, true)
	_ = nexus.ServeFrontend(nil, "web/dist")
	_ = nexus.IsDev()
	_ = nexus.DevStateDir()
	_ = nexus.NewNotifier()
	_ = nexus.LoadDotenvIfPresent(t.TempDir() + "/.env")
	_, _ = nexus.MapCRUDError(nexus.ErrForbidden)
	got := strings.Join(v2Names(), "\n")
	for _, want := range []string{
		"nexus.ServeFrontend", "nexus.IsDev", "nexus.DevStateDir", "nexus.NewNotifier",
		"nexus.LoadDotenvIfPresent", "nexus.MapCRUDError", "nexus.ErrForbidden",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
}
