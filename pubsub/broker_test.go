package pubsub

import (
	"reflect"
	"testing"

	"github.com/paulmanoni/nexus"
)

func TestEmbeddedTransportField(t *testing.T) {
	type good struct{ Transport }
	type alsoGood struct {
		name string
		Transport
	}
	type namedNotEmbedded struct{ T Transport }
	type bad struct{ X int }

	if _, err := embeddedTransportField(reflect.TypeFor[good]()); err != nil {
		t.Errorf("good: %v", err)
	}
	if _, err := embeddedTransportField(reflect.TypeFor[alsoGood]()); err != nil {
		t.Errorf("alsoGood: %v", err)
	}
	if _, err := embeddedTransportField(reflect.TypeFor[namedNotEmbedded]()); err == nil {
		t.Error("namedNotEmbedded: expected err (not embedded)")
	}
	if _, err := embeddedTransportField(reflect.TypeFor[bad]()); err == nil {
		t.Error("bad: expected err")
	}
}

// TestBrokerHandle_InterfaceFieldSet proves the reflective set works for
// an embedded *interface* field (the broker case, unlike DB/cache which
// embed a concrete pointer), and that promoted methods reach through.
func TestBrokerHandle_InterfaceFieldSet(t *testing.T) {
	type bus struct{ Transport }
	idx, err := embeddedTransportField(reflect.TypeFor[bus]())
	if err != nil {
		t.Fatal(err)
	}
	tr := NewInMemoryTransport()
	h := new(bus)
	reflect.ValueOf(h).Elem().Field(idx).Set(reflect.ValueOf(tr))
	if h.Transport == nil {
		t.Fatal("embedded Transport not set")
	}
	if err := h.Close(); err != nil { // promoted Close()
		t.Errorf("Close: %v", err)
	}
}

func TestBroker_BadTypePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for T without embedded Transport")
		}
	}()
	type bad struct{ X int }
	_ = Broker[bad]("x", func() (Transport, error) { return NewInMemoryTransport(), nil })
}

func TestBroker_ReturnsOption(t *testing.T) {
	type bus struct{ Transport }
	var opt nexus.Option = Broker[bus]("events", func() (Transport, error) {
		return NewInMemoryTransport(), nil
	}, WithBrokerDefault())
	if opt == nil {
		t.Fatal("Broker returned nil Option")
	}
}
