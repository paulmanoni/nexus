package view

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
)

// liveTags describe a live page for the dashboard: its kind, its type and
// its events with their argument types.
func (d *liveDef) liveTags() []nexus.RestOption {
	return []nexus.RestOption{
		nexus.Tag(registry.ViewTag, "live"),
		nexus.Tag(registry.ViewComponentTag, strings.TrimPrefix(d.t.String(), "*")),
		nexus.Tag(registry.ViewEventsTag, d.eventSummary()),
	}
}

// eventSummary is the live page's events as "Name(type, …)", sorted, the
// DI-injected parameters left out — what the browser sends.
func (d *liveDef) eventSummary() string {
	names := make([]string, 0, len(d.events))
	for name := range d.events {
		if isExported(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		m := d.events[name]
		args := make([]string, 0, len(m.args))
		for _, i := range m.args {
			args = append(args, m.fn.Type().In(i).String())
		}
		out = append(out, name+"("+strings.Join(args, ", ")+")")
	}
	return strings.Join(out, "; ")
}

// observedEvent runs one browser event as its own trace: a root span named
// after the live type and event, with how long the event and its render
// took and how much travelled back. Without a trace bus (no dashboard) it
// is the event alone.
func (d *liveDef) observedEvent(ctx context.Context, in *instance, ev liveEvent, render func(int, bool) liveReply) liveReply {
	if _, ok := trace.BusFromCtx(ctx); !ok {
		return d.event(ctx, in, ev, render)
	}
	typ := strings.TrimPrefix(d.t.String(), "*")
	name := typ + "." + ev.Event
	ctx, span, finish := trace.NewRootSpan(ctx, name, typ, name, "live",
		trace.Str("live.event", ev.Event))
	start := time.Now()
	reply := d.event(ctx, in, ev, render)
	span.Set("live.duration_ms", time.Since(start).Milliseconds())
	switch {
	case reply.HTML != "":
		span.Set("live.render", "full")
		span.Set("live.bytes", len(reply.HTML))
	case reply.Patch != nil:
		b, _ := json.Marshal(reply.Patch)
		span.Set("live.render", "patch")
		span.Set("live.bytes", len(b))
	}
	status := 200
	var err error
	if reply.Error != "" {
		status, err = 500, errString(reply.Error)
	} else if reply.Invalid {
		status = 422
	}
	finish(status, err)
	return reply
}

type errString string

func (e errString) Error() string { return string(e) }
