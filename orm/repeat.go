package orm

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"github.com/paulmanoni/nexus/v2/trace"
)

// RepeatWarn is how many times one request may run the same statement
// before nexus dev warns of an N+1: the statement reading related rows
// one parent at a time. 0 turns the warning off. It only watches under
// nexus dev (NEXUS_DEV set), inside a traced request.
var RepeatWarn = 5

var devMode = os.Getenv("NEXUS_DEV") != ""

// repeats counts statements per request trace, keeping the latest
// traces only.
var repeats = struct {
	mu     sync.Mutex
	counts map[string]map[string]int
	order  []string
}{counts: map[string]map[string]int{}}

const repeatTraces = 256

// noteRepeat counts q in ctx's request and warns once when it reaches
// RepeatWarn.
func noteRepeat(ctx context.Context, q string) {
	if !devMode || RepeatWarn <= 0 {
		return
	}
	sp, ok := trace.SpanFromCtx(ctx)
	if !ok || sp == nil || sp.TraceID == "" {
		return
	}
	repeats.mu.Lock()
	m, ok := repeats.counts[sp.TraceID]
	if !ok {
		m = map[string]int{}
		repeats.counts[sp.TraceID] = m
		repeats.order = append(repeats.order, sp.TraceID)
		if len(repeats.order) > repeatTraces {
			delete(repeats.counts, repeats.order[0])
			repeats.order = repeats.order[1:]
		}
	}
	m[q]++
	n := m[q]
	repeats.mu.Unlock()
	if n == RepeatWarn {
		const hint = "load related rows with SelectRelated or PrefetchRelated, or GraphRelation for a GraphQL field"
		slog.Default().WarnContext(ctx, "orm: the same query ran repeatedly in one request (N+1)",
			"times", n, "sql", q, "endpoint", sp.Endpoint, "hint", hint)
		sp.Set("orm.repeated", q)
		sp.Set("orm.hint", hint)
	}
}
