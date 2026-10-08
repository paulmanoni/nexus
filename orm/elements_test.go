package orm_test

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"github.com/paulmanoni/nexus/orm"
	"github.com/paulmanoni/nexus/orm/ormtest"
)

type Spot struct {
	ID   int64
	Kind string
}

// Memo keeps its lists as legacy text: ids comma-separated, labels too.
type Memo struct {
	ID    int64
	Refs  orm.CSV[int64]
	Tags  orm.CSV[string]
	Codes orm.JSON[[]string]
	Spots []Spot `orm:"elements:refs"`
	Coded []Spot `orm:"elements:codes,kind"`
}

var (
	Spots = orm.For[Spot]()
	Memos = orm.For[Memo]()
)

func elemOpen(t *testing.T) (context.Context, *Spot, *Spot, *Spot) {
	t.Helper()
	ctx := ormtest.Open(t, Letters, Spots, Memos)
	web1, web2, garden := &Spot{Kind: "web"}, &Spot{Kind: "web"}, &Spot{Kind: "garden"}
	ormtest.Seed(t, ctx, Spots, web1, web2, garden)
	return ctx, web1, web2, garden
}

func TestElementsJSON(t *testing.T) {
	ctx, web1, web2, garden := elemOpen(t)
	ormtest.Seed(t, ctx, Letters,
		&Letter{Ref: "A", Placings: orm.JSONOf([]int64{garden.ID, web1.ID})},
		&Letter{Ref: "B", Placings: orm.JSONOf([]int64{web2.ID})},
		&Letter{Ref: "C"})

	counts, err := Letters.Annotate("n", orm.Count("spots__id")).OrderBy("ref").Values[[]any]("ref", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(counts, [][]any{{"A", int64(2)}, {"B", int64(1)}, {"C", int64(0)}}) {
		t.Fatalf("counts = %v, %v", counts, err)
	}
	for _, c := range []struct {
		q    orm.Cond
		want []string
	}{
		{orm.Q{"spots__kind": "garden"}, []string{"A"}},
		{orm.Q{"spots__kind": "web"}, []string{"A", "B"}},
		{orm.Q{"spots__isnull": true}, []string{"C"}},
	} {
		got, err := Letters.Filter(c.q).OrderBy("ref").Values[string]("ref").All(ctx)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%v = %v, %v", c.q, got, err)
		}
	}
	web, err := Letters.Annotate("web", orm.FilteredRelation("spots", orm.Q{"kind": "web"})).
		Annotate("n", orm.Count("web__id").Distinct()).
		Filter(orm.Q{"n__gte": 1}).
		OrderBy("ref").Values[[]any]("ref", "n").All(ctx)
	if err != nil || !reflect.DeepEqual(web, [][]any{{"A", int64(1)}, {"B", int64(1)}}) {
		t.Fatalf("filtered relation over elements = %v, %v", web, err)
	}

	letters, err := Letters.PrefetchRelated("spots").OrderBy("ref").All(ctx)
	if err != nil || len(letters) != 3 {
		t.Fatal(letters, err)
	}
	if ids := spotIDs(letters[0].Spots); !slices.Equal(ids, []int64{garden.ID, web1.ID}) {
		t.Fatalf("prefetched in array order: %v", ids)
	}
	if len(letters[1].Spots) != 1 || letters[1].Spots[0].ID != web2.ID || len(letters[2].Spots) != 0 {
		t.Fatalf("prefetched %v %v", letters[1].Spots, letters[2].Spots)
	}
}

func TestElementsCSV(t *testing.T) {
	ctx, web1, web2, garden := elemOpen(t)
	ormtest.Seed(t, ctx, Memos,
		&Memo{Refs: orm.CSVOf(web1.ID, garden.ID), Tags: orm.CSVOf("a", "b"), Codes: orm.JSONOf([]string{"web"})},
		&Memo{Refs: orm.CSVOf[int64](), Codes: orm.JSONOf([]string{"garden"})})

	rows, err := Memos.OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(rows[0].Tags.V, []string{"a", "b"}) || rows[1].Tags.V != nil {
		t.Fatalf("CSV round trip %+v, %v", rows, err)
	}
	if !slices.Equal(rows[1].Refs.V, []int64{}) {
		t.Fatalf("an empty CSV reads as an empty list: %#v", rows[1].Refs.V)
	}

	counts, err := Memos.Annotate("n", orm.Count("spots__id")).OrderBy("id").Values[[]any]("id", "n").All(ctx)
	if err != nil || counts[0][1] != int64(2) || counts[1][1] != int64(0) {
		t.Fatalf("CSV counts = %v, %v", counts, err)
	}
	got, err := Memos.Filter(orm.Q{"spots__kind": "garden"}).Values[int64]("id").All(ctx)
	if err != nil || !slices.Equal(got, []int64{rows[0].ID}) {
		t.Fatalf("CSV filter = %v, %v", got, err)
	}

	// Legacy data with spaces around the commas still joins and prefetches.
	ormtest.Exec(t, ctx, "UPDATE memos SET refs = '"+itoa(web2.ID)+", "+itoa(garden.ID)+"' WHERE id = "+itoa(rows[0].ID))
	memos, err := Memos.PrefetchRelated("spots").OrderBy("id").All(ctx)
	if err != nil || !slices.Equal(spotIDs(memos[0].Spots), []int64{web2.ID, garden.ID}) {
		t.Fatalf("CSV prefetch %v, %v", memos, err)
	}

	// elements over a JSON array of strings, joined on a text column.
	coded, err := Memos.Annotate("n", orm.Count("coded__id")).OrderBy("id").Values[[]any]("id", "n").All(ctx)
	if err != nil || coded[0][1] != int64(2) || coded[1][1] != int64(1) {
		t.Fatalf("string elements = %v, %v", coded, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func spotIDs(ss []Spot) []int64 {
	out := make([]int64, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
