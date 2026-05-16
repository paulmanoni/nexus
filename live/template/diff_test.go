package template

import (
	"reflect"
	"testing"
)

func TestDiff_Identity(t *testing.T) {
	r := Rendered{S: []string{"a", "b"}, D: []any{"x"}}
	if d := DiffRendered(r, r); d != nil {
		t.Errorf("identical Rendereds should produce nil diff, got %#v", d)
	}
}

func TestDiff_SingleLeafChange(t *testing.T) {
	prev := Rendered{S: []string{"a", "b", "c"}, D: []any{"1", "2"}}
	next := Rendered{S: []string{"a", "b", "c"}, D: []any{"1", "twoNEW"}}

	d := DiffRendered(prev, next)
	want := Diff{"1": "twoNEW"}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func TestDiff_AllLeavesChange(t *testing.T) {
	prev := Rendered{S: []string{"a", "b", "c"}, D: []any{"1", "2"}}
	next := Rendered{S: []string{"a", "b", "c"}, D: []any{"X", "Y"}}

	d := DiffRendered(prev, next)
	want := Diff{"0": "X", "1": "Y"}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func TestDiff_NestedRenderedSparse(t *testing.T) {
	// Same inner statics, only an inner dynamic flips. The outer
	// slot should get a nested Diff, not a full inner Rendered.
	innerStatics := []string{"<b>", "</b>"}
	prev := Rendered{
		S: []string{"<p>", "</p>"},
		D: []any{Rendered{S: innerStatics, D: []any{"old"}}},
	}
	next := Rendered{
		S: []string{"<p>", "</p>"},
		D: []any{Rendered{S: innerStatics, D: []any{"new"}}},
	}

	d := DiffRendered(prev, next)
	want := Diff{"0": Diff{"0": "new"}}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func TestDiff_NestedRenderedDifferentStaticsShipsFull(t *testing.T) {
	// Different inner statics → can't sparse-diff (client lacks
	// the new statics). Slot should carry the full Rendered.
	prev := Rendered{
		S: []string{"<p>", "</p>"},
		D: []any{Rendered{S: []string{"<b>", "</b>"}, D: []any{"x"}}},
	}
	nextInner := Rendered{S: []string{"<i>", "</i>"}, D: []any{"x"}}
	next := Rendered{S: []string{"<p>", "</p>"}, D: []any{nextInner}}

	d := DiffRendered(prev, next)
	want := Diff{"0": nextInner}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func TestDiff_NilToValue(t *testing.T) {
	// nl-if branch flipped from untaken → taken: ships the new value.
	prev := Rendered{S: []string{"a", "b"}, D: []any{nil}}
	next := Rendered{S: []string{"a", "b"}, D: []any{"hello"}}

	d := DiffRendered(prev, next)
	want := Diff{"0": "hello"}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func TestDiff_ValueToNil(t *testing.T) {
	// nl-if went taken → untaken: ships empty-string sentinel so the
	// client clears the slot without ambiguous null handling.
	prev := Rendered{S: []string{"a", "b"}, D: []any{"hello"}}
	next := Rendered{S: []string{"a", "b"}, D: []any{nil}}

	d := DiffRendered(prev, next)
	want := Diff{"0": ""}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("got %#v want %#v", d, want)
	}
}

func mkComp(statics []string, kv ...string) Comprehension {
	// kv: alternating key, single-dynamic-value pairs.
	rows := make([]Row, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		rows = append(rows, Row{Key: kv[i], D: []any{kv[i+1]}})
	}
	return Comprehension{S: statics, Rows: rows}
}

func TestDiff_ComprehensionIdenticalIsNil(t *testing.T) {
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b"),
	}}
	if d := DiffRendered(prev, next); d != nil {
		t.Errorf("identical comps should produce nil diff; got %#v", d)
	}
}

func TestDiff_ComprehensionUpdate(t *testing.T) {
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b", "3", "c"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "B-NEW", "3", "c"),
	}}

	d := DiffRendered(prev, next)
	got, ok := d["0"].(CompDiff)
	if !ok {
		t.Fatalf("expected CompDiff in slot 0, got %T", d["0"])
	}
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(got.Order, want) {
		t.Errorf("Order: got %v want %v", got.Order, want)
	}
	wantUpdates := map[string]Diff{"2": {"0": "B-NEW"}}
	if !reflect.DeepEqual(got.Updates, wantUpdates) {
		t.Errorf("Updates: got %#v want %#v", got.Updates, wantUpdates)
	}
	if got.Inserts != nil {
		t.Errorf("Inserts should be nil; got %#v", got.Inserts)
	}
}

func TestDiff_ComprehensionInsert(t *testing.T) {
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b"),
	}}

	got := DiffRendered(prev, next)["0"].(CompDiff)
	if want := []string{"1", "2"}; !reflect.DeepEqual(got.Order, want) {
		t.Errorf("Order: got %v want %v", got.Order, want)
	}
	wantInserts := map[string][]any{"2": {"b"}}
	if !reflect.DeepEqual(got.Inserts, wantInserts) {
		t.Errorf("Inserts: got %#v want %#v", got.Inserts, wantInserts)
	}
	if got.Updates != nil {
		t.Errorf("Updates should be nil; got %#v", got.Updates)
	}
}

func TestDiff_ComprehensionRemoveInferredFromOrder(t *testing.T) {
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b", "3", "c"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "3", "c"),
	}}

	got := DiffRendered(prev, next)["0"].(CompDiff)
	if want := []string{"1", "3"}; !reflect.DeepEqual(got.Order, want) {
		t.Errorf("Order: got %v want %v (key '2' must be absent)", got.Order, want)
	}
	if got.Updates != nil {
		t.Errorf("Updates should be nil for pure removal; got %#v", got.Updates)
	}
	if got.Inserts != nil {
		t.Errorf("Inserts should be nil for pure removal; got %#v", got.Inserts)
	}
}

func TestDiff_ComprehensionReorderOnly(t *testing.T) {
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b", "3", "c"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "3", "c", "1", "a", "2", "b"),
	}}

	got := DiffRendered(prev, next)["0"].(CompDiff)
	if want := []string{"3", "1", "2"}; !reflect.DeepEqual(got.Order, want) {
		t.Errorf("Order: got %v want %v", got.Order, want)
	}
	if got.Updates != nil {
		t.Errorf("Pure reorder should not emit Updates; got %#v", got.Updates)
	}
	if got.Inserts != nil {
		t.Errorf("Pure reorder should not emit Inserts; got %#v", got.Inserts)
	}
}

func TestDiff_ComprehensionMixed(t *testing.T) {
	// Removed "1", kept "2" (changed), kept "3" (unchanged), inserted "4".
	// Final order: 3, 2, 4.
	st := []string{"<li>", "</li>"}
	prev := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "1", "a", "2", "b", "3", "c"),
	}}
	next := Rendered{S: []string{"<ul>", "</ul>"}, D: []any{
		mkComp(st, "3", "c", "2", "B-NEW", "4", "d"),
	}}

	got := DiffRendered(prev, next)["0"].(CompDiff)

	if want := []string{"3", "2", "4"}; !reflect.DeepEqual(got.Order, want) {
		t.Errorf("Order: got %v want %v", got.Order, want)
	}
	wantUpdates := map[string]Diff{"2": {"0": "B-NEW"}}
	if !reflect.DeepEqual(got.Updates, wantUpdates) {
		t.Errorf("Updates: got %#v want %#v", got.Updates, wantUpdates)
	}
	wantInserts := map[string][]any{"4": {"d"}}
	if !reflect.DeepEqual(got.Inserts, wantInserts) {
		t.Errorf("Inserts: got %#v want %#v", got.Inserts, wantInserts)
	}
}

func TestDiff_StaticsMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for mismatched statics")
		}
	}()
	prev := Rendered{S: []string{"a", "b"}, D: []any{"x"}}
	next := Rendered{S: []string{"a", "c"}, D: []any{"x"}}
	_ = DiffRendered(prev, next)
}

func TestDiff_DynamicCountMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for dynamic count mismatch")
		}
	}()
	prev := Rendered{S: []string{"a", "b"}, D: []any{"x"}}
	next := Rendered{S: []string{"a", "b"}, D: []any{"x", "y"}}
	_ = DiffRendered(prev, next)
}
