package nexus

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/db"
)

// testDBHandle is the kind of one-line marker type users declare.
type testDBHandle struct{ *db.Manager }

func TestDBHandleFieldIndex(t *testing.T) {
	type good struct{ *db.Manager }
	type alsoGood struct {
		name string
		*db.Manager
	}
	type namedNotEmbedded struct{ M *db.Manager }
	type noManager struct{ X int }

	if _, err := embeddedFieldIndex(reflect.TypeFor[good](), managerPtrType); err != nil {
		t.Errorf("good: unexpected err: %v", err)
	}
	if _, err := embeddedFieldIndex(reflect.TypeFor[alsoGood](), managerPtrType); err != nil {
		t.Errorf("alsoGood: unexpected err: %v", err)
	}
	if _, err := embeddedFieldIndex(reflect.TypeFor[namedNotEmbedded](), managerPtrType); err == nil {
		t.Error("namedNotEmbedded: expected err (field is not embedded)")
	}
	if _, err := embeddedFieldIndex(reflect.TypeFor[noManager](), managerPtrType); err == nil {
		t.Error("noManager: expected err")
	}
}

func TestDatabase_BadTypePanicsAtWiring(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for T without embedded *db.Manager")
		}
	}()
	type bad struct{ X int }
	_ = Database[bad]("x", func() db.Config { return db.Config{Driver: db.SQLite} })
}

func TestDatabase_NilBuildPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil build func")
		}
	}()
	_ = Database[testDBHandle]("x", nil)
}

// TestDatabase_WiresInjectableHandle runs the option through a real fx
// graph (with a real *App for resource registration) and checks the
// handle is injectable with its embedded manager set and lifecycle
// bound. Uses sqlite :memory: so it needs no external server or cgo.
func TestDatabase_WiresInjectableHandle(t *testing.T) {
	app := New(Config{})

	var got *testDBHandle
	opt := Database[testDBHandle]("testdb", func() db.Config {
		return db.Config{Driver: db.SQLite, Database: "file::memory:?cache=shared"}
	}, WithDatabaseDefault())

	fxapp := fx.New(
		fx.Supply(app),
		fx.Supply(zap.NewNop()),
		opt.nexusOption(),
		fx.Populate(&got),
	)

	ctx := context.Background()
	if err := fxapp.Start(ctx); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	defer func() { _ = fxapp.Stop(ctx) }()

	if got == nil {
		t.Fatal("*testDBHandle was not injected")
	}
	if got.Manager == nil {
		t.Fatal("embedded *db.Manager was not set on the handle")
	}
	if got.Driver() != db.SQLite { // promoted from *db.Manager
		t.Errorf("Driver() = %q, want sqlite", got.Driver())
	}
}

// Benchmarks isolating the only cost difference between Database[T] and a
// hand-written provider: the one-time reflective field-set at
// construction. The request hot-path (promoted method calls on *T) is
// identical for both, shown by BenchmarkDBHandle_PromotedCall.

func BenchmarkDBHandle_ReflectiveConstruct(b *testing.B) {
	m := db.NewManager(db.Config{Driver: db.SQLite}) // no Start: no IO
	idx, err := embeddedFieldIndex(reflect.TypeFor[testDBHandle](), managerPtrType)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := newHandle[testDBHandle](idx, m) // the exact code Database uses
		_ = h
	}
}

func BenchmarkDBHandle_DirectConstruct(b *testing.B) {
	m := db.NewManager(db.Config{Driver: db.SQLite})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h := &testDBHandle{m} // the hand-written equivalent
		_ = h
	}
}

func BenchmarkDBHandle_PromotedCall(b *testing.B) {
	m := db.NewManager(db.Config{Driver: db.SQLite})
	h := &testDBHandle{m}
	b.ReportAllocs()
	b.ResetTimer()
	var d db.Driver
	for i := 0; i < b.N; i++ {
		d = h.Driver() // promoted from *db.Manager — request-path access
	}
	_ = d
}
