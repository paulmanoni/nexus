package config

import "testing"

// Packages reading the same table may each declare it with the same type;
// every handle sees the file. A different type is still a clash.
func TestSectionSharedDeclaration(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	a := Section[map[string]any]("shared_tbl")
	b := Section[map[string]any]("shared_tbl")
	if _, err := Parse([]byte("[shared_tbl]\nk = \"v\"\n"), "test"); err != nil {
		t.Fatal(err)
	}
	if a.Get()["k"] != "v" || b.Get()["k"] != "v" {
		t.Fatalf("a=%v b=%v", a.Get(), b.Get())
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a different type for the same table should panic")
		}
	}()
	Section[struct{ K string }]("shared_tbl")
}
