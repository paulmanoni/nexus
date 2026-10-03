package viewgen

import (
	"strings"
	"testing"
)

// v1.80 reads Go's directive form (//nexus:page) as well as //@page, and the
// two register the same page.
func TestDirectivePrefixes(t *testing.T) {
	directive := strings.NewReplacer("//@page", "//nexus:page", "//@auth", "//nexus:auth").Replace(page)
	var gens []string
	for _, src := range []string{page, directive} {
		res, err := File("app/page.templ", src, pkg)
		if err != nil {
			t.Fatal(err)
		}
		gen, err := Registrations([]*Result{res}, pkg, nil)
		if err != nil {
			t.Fatal(err)
		}
		gens = append(gens, string(gen))
	}
	if gens[0] != gens[1] || !strings.Contains(gens[1], `view.Page("GET", "/", Home, auth.Required())`) {
		t.Fatalf("//nexus: and //@ differ:\n%s\n---\n%s", gens[0], gens[1])
	}
}
