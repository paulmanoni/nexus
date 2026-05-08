package graph

import (
	"reflect"
	"testing"

	"github.com/graphql-go/graphql"
)

// benchPayload is a representative response struct: 8 scalar fields
// covering the common kinds. This stays small on purpose so the
// benchmark measures per-field resolver dispatch, not megabyte-sized
// JSON marshaling.
type benchPayload struct {
	ID        int     `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Active    bool    `json:"active"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"createdAt"`
	UpdatedAt string  `json:"updatedAt"`
	Notes     string  `json:"notes"`
}

// embeddedBase + benchEmbeddedPayload exercise the field-promotion
// path. The optimization has to handle promoted fields correctly —
// pre-change FieldByName followed promotion silently; post-change
// FieldByIndex needs the full index path threaded through embeddings.
type embeddedBase struct {
	BaseID  int    `json:"baseId"`
	BaseTag string `json:"baseTag"`
}

type benchEmbeddedPayload struct {
	embeddedBase
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

// BenchmarkFieldResolve_Flat measures the cost of resolving every
// field on a flat (non-embedded) struct. This is the dominant case
// in real apps — a Response[T] wrapper around a domain entity.
func BenchmarkFieldResolve_Flat(b *testing.B) {
	gen := NewFieldGenerator[benchPayload]()
	fields := gen.generateFields(reflect.TypeOf(benchPayload{}))
	src := benchPayload{
		ID: 42, Name: "alice", Email: "a@b.c", Active: true,
		Score: 3.14, CreatedAt: "2026-01-01", UpdatedAt: "2026-01-02", Notes: "n",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, f := range fields {
			_, _ = f.Resolve(graphql.ResolveParams{Source: src})
		}
	}
}

// BenchmarkFieldResolve_Embedded measures the same dispatch through
// a field-promotion chain, which is the case where naive
// FieldByIndex breaks (it'd index into the wrong slot if the prefix
// isn't threaded correctly).
func BenchmarkFieldResolve_Embedded(b *testing.B) {
	gen := NewFieldGenerator[benchEmbeddedPayload]()
	fields := gen.generateFields(reflect.TypeOf(benchEmbeddedPayload{}))
	src := benchEmbeddedPayload{
		embeddedBase: embeddedBase{BaseID: 7, BaseTag: "t"},
		Name:         "alice", Active: true,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, f := range fields {
			_, _ = f.Resolve(graphql.ResolveParams{Source: src})
		}
	}
}