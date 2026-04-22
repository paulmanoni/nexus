package multi

import (
	"context"
	"sync"
	"testing"
)

type fakeDB struct{ name string }

func TestRegistry_FirstRegisteredIsImplicitDefault(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("main", &fakeDB{"main"})
	r.Register("questions", &fakeDB{"questions"})

	if r.DefaultName() != "main" {
		t.Fatalf("default: want main, got %q", r.DefaultName())
	}
	if r.Using("").name != "main" {
		t.Fatalf("Using(\"\"): want main, got %q", r.Using("").name)
	}
	if r.Default().name != "main" {
		t.Fatalf("Default(): got %q", r.Default().name)
	}
}

func TestRegistry_AsDefaultOverridesFirstWins(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("main", &fakeDB{"main"})
	r.Register("questions", &fakeDB{"questions"}, AsDefault[*fakeDB]())

	if r.DefaultName() != "questions" {
		t.Fatalf("AsDefault should win: got %q", r.DefaultName())
	}
	if r.Using("").name != "questions" {
		t.Fatalf("Using(\"\"): %q", r.Using("").name)
	}
}

func TestRegistry_SetDefault(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("main", &fakeDB{"main"})
	r.Register("questions", &fakeDB{"questions"})
	r.SetDefault("questions")
	if r.Using("").name != "questions" {
		t.Fatalf("SetDefault didn't take")
	}
}

func TestRegistry_SetDefault_PanicsOnUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unknown name")
		}
	}()
	New[*fakeDB]().SetDefault("nope")
}

func TestRegistry_UsingByName(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("main", &fakeDB{"main"})
	r.Register("qb", &fakeDB{"qb"})
	if r.Using("qb").name != "qb" {
		t.Fatalf("wrong instance for qb")
	}
}

func TestRegistry_Has(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("main", &fakeDB{"main"})
	if !r.Has("main") {
		t.Fatal("Has(main)")
	}
	if r.Has("nope") {
		t.Fatal("Has(nope) should be false")
	}
	if !r.Has("") {
		t.Fatal("Has(\"\") should be true when a default exists")
	}
}

func TestRegistry_NamesAndEach(t *testing.T) {
	r := New[*fakeDB]()
	r.Register("zeta", &fakeDB{"zeta"})
	r.Register("alpha", &fakeDB{"alpha"})
	r.Register("mu", &fakeDB{"mu"})
	want := []string{"alpha", "mu", "zeta"}
	got := r.Names()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names: %v, want %v", got, want)
		}
	}
	var seen []string
	r.Each(func(name string, v *fakeDB) {
		if v.name != name {
			t.Fatalf("Each inconsistency: key=%s value=%s", name, v.name)
		}
		seen = append(seen, name)
	})
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("Each order: %v", seen)
		}
	}
}

func TestRegistry_ConcurrentRegisterAndUsing(t *testing.T) {
	r := New[*fakeDB]()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "db" + string(rune('a'+i%5))
			r.Register(name, &fakeDB{name})
			_ = r.Using(name)
			_ = r.Using("")
		}(i)
	}
	wg.Wait()
	if r.Len() != 5 {
		t.Fatalf("expected 5 unique names, got %d", r.Len())
	}
}

func TestRegistry_OnUse_FiresPerCall(t *testing.T) {
	r := New[*fakeDB]().
		Register("main", &fakeDB{"main"}).
		Register("questions", &fakeDB{"questions"})

	type hit struct {
		ctxVal any
		name   string
	}
	var mu sync.Mutex
	var hits []hit
	r.OnUse(func(ctx context.Context, name string) {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, hit{ctx.Value("k"), name})
	})

	ctx := context.WithValue(context.Background(), "k", "v1")
	if v := r.UsingCtx(ctx, "questions"); v.name != "questions" {
		t.Fatalf("UsingCtx returned wrong instance: %q", v.name)
	}

	if _ = r.Using("main"); len(hits) != 1 {
		t.Fatalf("Using (no ctx) should not fire hook; got %d hits", len(hits))
	}

	if len(hits) != 1 || hits[0].name != "questions" || hits[0].ctxVal != "v1" {
		t.Fatalf("hits: %+v", hits)
	}
}

func TestRegistry_OnUse_ReplacesHook(t *testing.T) {
	r := New[*fakeDB]().Register("main", &fakeDB{"main"})
	var a, b int
	r.OnUse(func(context.Context, string) { a++ })
	r.OnUse(func(context.Context, string) { b++ })
	r.UsingCtx(context.Background(), "main")
	if a != 0 || b != 1 {
		t.Fatalf("second OnUse should replace first: a=%d b=%d", a, b)
	}
}
