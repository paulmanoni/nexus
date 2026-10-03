package v2notice

import (
	"bytes"
	"strings"
	"testing"
)

func TestFramework(t *testing.T) {
	cases := map[string]bool{
		"github.com/paulmanoni/nexus.Get[...]":                       true,
		"github.com/paulmanoni/nexus.(*App).UseVolume":               true,
		"github.com/paulmanoni/nexus/extension/auth.LoginEndpoint":   true,
		"github.com/paulmanoni/nexus/view.Assets.func1":              true,
		"github.com/paulmanoni/nexus/examples/notes/notes.NewStore":  false,
		"github.com/paulmanoni/nexus/view/example/pets.(*Board).Add": false,
		"github.com/paulmanoni/nexus_test.TestX":                     false,
		"github.com/paulmanoni/nexusapp/users.NewUsers":              false,
		"main.main": false,
		"example.com/shop/internal/orders.(*Service).Create-fm":             false,
		"github.com/paulmanoni/nexus/extension/inertia.(*Engine).render":    true,
		"github.com/paulmanoni/nexus/examples/graphapp.NewListOrders.func1": false,
	}
	for fn, want := range cases {
		if got := Framework(fn); got != want {
			t.Errorf("Framework(%q) = %v, want %v", fn, got, want)
		}
	}
}

func TestFlushOnceThenLate(t *testing.T) {
	t.Setenv("NEXUS_DEV", "1")
	t.Setenv("NEXUS_V2_NOTICES", "")
	var buf bytes.Buffer
	prev := Output
	Output = &buf
	Reset()
	defer func() { Output = prev; Reset() }()

	Note("a", "A2")
	Note("a", "A2")
	NoteAt("b", "B2", "x.go:3")
	if len(Items()) != 2 {
		t.Fatalf("items = %+v", Items())
	}
	Flush()
	out := buf.String()
	if !strings.Contains(out, "2 v1 API(s)") || !strings.Contains(out, "b  (x.go:3)") || !strings.Contains(out, "v2: B2") {
		t.Fatalf("block:\n%s", out)
	}
	buf.Reset()
	Flush()
	Note("c", "C2")
	Note("c", "C2")
	if strings.Count(buf.String(), "- c") != 1 {
		t.Fatalf("late:\n%s", buf.String())
	}
}

func TestDisabledOutsideDev(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	Reset()
	defer Reset()
	Note("a", "A2")
	if Enabled() || len(Items()) != 0 {
		t.Fatal("recorded outside dev")
	}
}
