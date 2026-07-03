package registry

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/internal/golden"
)

// The framework has no standalone GraphQL-SDL string emitter — the GraphQL
// schema text is produced by graphql-go's introspection, whose field ordering is
// map-based and non-deterministic (unsuitable for a golden file). WalkType is
// the deterministic layer that BOTH the GraphQL schema and the client SDK derive
// from: it reflects Go types into the TypeRef/NamedType schema. Golden-testing it
// pins the schema-extraction contract — the meaningful, stable equivalent of an
// SDL snapshot. encoding/json sorts string-map keys, so the Refs map serializes
// deterministically; struct fields keep declaration order.

type goldenAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

type goldenProfile struct {
	Bio     string         `json:"bio"`
	Age     *int           `json:"age,omitempty"` // pointer → optional
	Address goldenAddress  `json:"address"`       // nested named ref
	Scores  map[string]int `json:"scores"`        // map branch
}

type goldenUser struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Active    bool           `json:"active"`
	Balance   float64        `json:"balance"`
	Tags      []string       `json:"tags"`      // slice branch
	Profile   goldenProfile  `json:"profile"`   // nested named ref
	Friends   []*goldenUser  `json:"friends"`   // self-referential cycle + slice of ptr
	Avatar    []byte         `json:"avatar"`    // []byte → string
	CreatedAt time.Time      `json:"createdAt"` // time.Time → string
	Meta      map[string]any `json:"meta"`      // map of any
}

// TestWalkType_Golden snapshots the full schema produced by reflecting a rich Go
// type — exercising primitives, pointer/optional, slice, map, named-type refs,
// a self-referential cycle, []byte, and time.Time. Regenerate intentionally
// with: UPDATE_GOLDEN=1 go test ./registry/...
func TestWalkType_Golden(t *testing.T) {
	refs := map[string]NamedType{}
	root := WalkType(reflect.TypeOf(goldenUser{}), refs)

	out, err := json.MarshalIndent(struct {
		Root TypeRef              `json:"root"`
		Refs map[string]NamedType `json:"refs"`
	}{Root: root, Refs: refs}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	golden.Assert(t, append(out, '\n'), "walktype_user.json")
}
