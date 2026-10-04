package view

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
)

var refuseConnections atomic.Bool

func init() {
	nexus.RegisterConnectionCheck(func(ctx context.Context) error {
		if refuseConnections.Load() {
			return nexus.Err(nexus.Unauthenticated, "signed out")
		}
		return nil
	})
}

// A live page runs the connection checks before each event: once one
// fails, the event gets the error and the socket closes.
func TestLiveConnectionCheck(t *testing.T) {
	srv := bootLive(t)
	a := dialLive(t, srv, "/count/ana/_live")
	reply(t, a)
	raw, _ := json.Marshal(5)
	if err := a.WriteJSON(liveEvent{Event: "Add", Args: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	reply(t, a)

	refuseConnections.Store(true)
	defer refuseConnections.Store(false)
	if err := a.WriteJSON(liveEvent{Event: "Add", Args: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, a); r.Error != "signed out" {
		t.Fatalf("a refused event = %+v", r)
	}
	_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
	var r liveReply
	if err := a.ReadJSON(&r); err == nil && (r.Tree != nil || r.Error != "") {
		t.Fatalf("the socket stays open: %+v", r)
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the socket did not close")
	}
}
