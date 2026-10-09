package view

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/dev"
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
	typ := strings.TrimPrefix(d.t.String(), "*")
	name := typ + "." + ev.Event
	ctx, tracked := dev.Track(ctx, in.devPage, "LIVE", name)
	if _, ok := trace.BusFromCtx(ctx); !ok {
		reply := d.event(ctx, in, ev, render)
		tracked(replyStatus(reply))
		return reply
	}
	ctx, span, finish := trace.NewRootSpan(ctx, name, typ, name, "live",
		trace.Str("live.event", ev.Event))
	start := time.Now()
	reply := d.event(ctx, in, ev, render)
	span.Set("live.duration_ms", time.Since(start).Milliseconds())
	if reply.Tree != nil {
		b, _ := marshal(reply.Tree)
		span.Set("live.render", map[bool]string{true: "full", false: "diff"}[reply.Full])
		span.Set("live.bytes", len(b))
	}
	status, err := replyStatus(reply)
	finish(status, err)
	tracked(status, err)
	return reply
}

// replyStatus is an event's outcome as an HTTP status.
func replyStatus(reply liveReply) (int, error) {
	switch {
	case reply.Error != "":
		return 500, errString(reply.Error)
	case reply.Invalid:
		return 422, nil
	}
	return 200, nil
}

// span starts a trace of its own for socket work that isn't a browser
// event — the connection's Mount, a patch, a broadcast — so it isn't
// counted with every other message the connection has carried since its
// upgrade request. Under nexus dev it is an entry of the page's toolbar.
func (d *liveDef) span(ctx context.Context, in *instance, op string) (context.Context, func(error)) {
	typ := strings.TrimPrefix(d.t.String(), "*")
	ctx, tracked := dev.Track(ctx, in.devPage, "LIVE", typ+"."+op)
	ctx, _, finish := trace.NewRootSpan(ctx, typ+"."+op, typ, typ+"."+op, "live")
	return ctx, func(err error) {
		status := 200
		if err != nil {
			status = 500
		}
		finish(status, err)
		tracked(status, err)
	}
}

// pageSpan is span for work that shows a page — the connection's Mount, a
// patch's Update: under nexus dev the page's toolbar entry, by its URL
// path, which the toolbar pins once the browser shows it — a live
// navigation or a patch (a tab, a filter in the URL) is a page without a
// page load.
func (d *liveDef) pageSpan(ctx context.Context, in *instance, op string) (context.Context, func(error)) {
	typ := strings.TrimPrefix(d.t.String(), "*")
	path := typ
	if in.url != nil {
		path = in.url.Path
	}
	ctx, tracked := dev.TrackPage(ctx, in.devPage, "LIVE", path)
	ctx, _, finish := trace.NewRootSpan(ctx, typ+"."+op, typ, typ+"."+op, "live")
	return ctx, func(err error) {
		status := 200
		if err != nil {
			status = 500
		}
		finish(status, err)
		tracked(status, err)
	}
}

func (d *liveDef) inform(ctx context.Context, in *instance, msg Message, send func(liveReply) bool) bool {
	ctx, done := d.span(ctx, in, "Info")
	defer done(nil)
	return in.inform(ctx, msg, send)
}

type errString string

func (e errString) Error() string { return string(e) }
