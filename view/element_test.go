package view

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScalarFromString(t *testing.T) {
	n := reflect.New(reflect.TypeOf(0))
	if !scalarFromString(json.RawMessage(`"42"`), n) || n.Elem().Int() != 42 {
		t.Errorf("int from \"42\": %v", n.Elem())
	}
	ok := reflect.New(reflect.TypeOf(false))
	if !scalarFromString(json.RawMessage(`"true"`), ok) || !ok.Elem().Bool() {
		t.Error("bool from \"true\"")
	}
	ptr := reflect.New(reflect.TypeOf((*int)(nil)))
	if !scalarFromString(json.RawMessage(`""`), ptr) || !ptr.Elem().IsNil() {
		t.Error("an empty string leaves a pointer nil")
	}
	for _, raw := range []string{`""`, `"x"`, `5`} {
		if scalarFromString(json.RawMessage(raw), reflect.New(reflect.TypeOf(0))) {
			t.Errorf("%s decoded into an int", raw)
		}
	}
	if scalarFromString(json.RawMessage(`"a"`), reflect.New(reflect.TypeOf(struct{}{}))) {
		t.Error("a string decoded into a struct")
	}
}
