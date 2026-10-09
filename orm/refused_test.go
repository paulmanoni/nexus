package orm_test

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/orm"
	"github.com/paulmanoni/nexus/orm/ormtest"
	"github.com/paulmanoni/nexus/v2/trace"
)

func TestRefusedQueriesAreTraced(t *testing.T) {
	orm.SetDevMode(t, true)
	ctx := ormtest.Open(t, Tickets, OldTickets)
	bus := trace.NewBus(200)
	ctx, root, end := trace.NewRootSpan(trace.WithBus(ctx, bus), "GET /tickets", "tickets", "/tickets", "rest")

	OldTickets.Filter(orm.Q{"status": 2}).Count(ctx)                              // refused: an int for a per-schema field
	OldTickets.Values[string]("status").All(ctx)                                  // refused: read into a string
	OldTickets.Filter(orm.Q{"status": TicketOpen}).Count(ctx)                     // runs
	OldTickets.Get(ctx, orm.Q{"id": 999})                                         // runs, finds nothing: not a refusal
	OldTickets.Filter(orm.Q{"status__in": []int{6}}).Values[int64]("id").All(ctx) // refused
	end(200, nil)

	var refused, ran int
	for _, sp := range bus.Spans(root.TraceID) {
		if sp.Name != "sql" {
			continue
		}
		if sp.Attrs["orm.refused"] == true {
			refused++
			if !strings.Contains(sp.Error, "differs per schema") || !strings.Contains(sp.Attrs["sql"].(string), "Ticket query the ORM refused") {
				t.Errorf("refusal span %+v", sp)
			}
			continue
		}
		ran++
	}
	if refused != 3 || ran != 2 {
		t.Fatalf("refused %d, ran %d", refused, ran)
	}

	// Outside nexus dev nothing is recorded for a refusal.
	orm.SetDevMode(t, false)
	ctx2, root2, end2 := trace.NewRootSpan(trace.WithBus(context.Background(), bus), "GET /x", "x", "/x", "rest")
	OldTickets.Filter(orm.Q{"status": 2}).Count(ctx2)
	end2(200, nil)
	for _, sp := range bus.Spans(root2.TraceID) {
		if sp.Name == "sql" {
			t.Fatalf("recorded outside nexus dev: %+v", sp)
		}
	}
}
