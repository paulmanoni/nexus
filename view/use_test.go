package view

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type searchState struct {
	Query *Signal[string]
	Page  *Signal[int]
	Label string
}

type petService struct{ name string }

func expose(v any) { exposed[reflect.TypeOf(v)] = v }

// A state struct from DI is per page: components in one render share its
// signals, another render starts from the DI defaults, a shard re-render
// starts from the browser's values, and the DI instance never changes.
func TestUseState(t *testing.T) {
	defaults := &searchState{Query: Initial("cats"), Label: "search"}
	expose(defaults)
	svc := &petService{name: "pets"}
	expose(svc)

	page := withRender(context.Background(), &render{})
	a, b := Use[*searchState](page), Use[*searchState](page)
	if a != b || a.Query.ID() == "" || a.Query.Get() != "cats" || a.Page.Get() != 0 || a.Label != "search" {
		t.Fatalf("one page shares one copy with the defaults: %+v", a)
	}
	if Use[*petService](page) != svc {
		t.Fatal("a service comes back as is")
	}

	restored := withRender(context.Background(), &render{restore: map[string]json.RawMessage{
		a.Query.ID(): json.RawMessage(`"dogs"`),
	}})
	c := Use[*searchState](restored)
	if c == a || c.Query.ID() != a.Query.ID() || c.Query.Get() != "dogs" {
		t.Fatalf("another render gets its own copy, same ids, browser value: %+v", c.Query)
	}
	if defaults.Query.Get() != "cats" || defaults.Query.ID() != "" {
		t.Fatal("the DI instance must not change")
	}
}
