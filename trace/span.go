package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Attr is a key/value annotation attached to a span. Surfaced in the
// dashboard's trace waterfall (attrs panel per span).
type Attr struct {
	Key string
	Val any
}

// Str / Int / Bool / Any are constructor sugar for the common attr types.
// They exist so call sites stay short:
//
//	trace.StartSpan(ctx, "db.users.get", trace.Str("id", userID))
func Str(k, v string) Attr       { return Attr{Key: k, Val: v} }
func Int(k string, v int64) Attr { return Attr{Key: k, Val: v} }
func Bool(k string, v bool) Attr { return Attr{Key: k, Val: v} }
func Any(k string, v any) Attr   { return Attr{Key: k, Val: v} }

// Span represents one unit of work inside a trace. The root span is minted by
// the request Middleware (HTTP ingress); child spans are pushed with StartSpan
// and closed with End.
//
// Span carries its bus pointer so End can publish without re-reading ctx — a
// handler may call End after the request's ctx has expired (deferred cleanup).
type Span struct {
	TraceID  string
	SpanID   string
	ParentID string
	Name     string
	Service  string
	Endpoint string
	Start    time.Time
	// Remote is true when this span's TraceID came from an inbound
	// W3C traceparent header (continuing an upstream trace) rather
	// than being minted fresh at ingress.
	Remote bool

	mu    sync.Mutex
	attrs map[string]any
	bus   *Bus
	ended bool
}

// Set records one attribute on the span. Safe for concurrent use.
func (s *Span) Set(key string, val any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.attrs == nil {
		s.attrs = map[string]any{}
	}
	s.attrs[key] = val
	s.mu.Unlock()
}

// SetAttrs applies a batch of attributes.
func (s *Span) SetAttrs(attrs ...Attr) {
	if s == nil || len(attrs) == 0 {
		return
	}
	s.mu.Lock()
	if s.attrs == nil {
		s.attrs = map[string]any{}
	}
	for _, a := range attrs {
		s.attrs[a.Key] = a.Val
	}
	s.mu.Unlock()
}

func (s *Span) snapshotAttrs() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attrs) == 0 {
		return nil
	}
	cp := make(map[string]any, len(s.attrs))
	for k, v := range s.attrs {
		cp[k] = v
	}
	return cp
}

// StartSpan pushes a child span under whatever span is currently in ctx and
// returns a derived ctx carrying the child. If ctx has no parent span or no
// bus, the returned span is a no-op stub — End is still safe to call.
//
// Typical use:
//
//	ctx, span := trace.StartSpan(ctx, "rabbit.publish", trace.Str("topic", t))
//	err := ch.Publish(ctx, payload)
//	span.End(err)
func StartSpan(ctx context.Context, name string, attrs ...Attr) (context.Context, *Span) {
	parent, hasParent := SpanFromCtx(ctx)
	bus, _ := BusFromCtx(ctx)
	child := &Span{
		Name:   name,
		Start:  time.Now(),
		SpanID: newSpanID(),
		bus:    bus,
	}
	if hasParent && parent != nil {
		child.TraceID = parent.TraceID
		child.ParentID = parent.SpanID
		child.Service = parent.Service
		child.Endpoint = parent.Endpoint
	} else {
		// Orphan span — mint a fresh trace so it's still renderable on
		// its own. Happens when StartSpan is called outside an HTTP
		// request (a cron, a worker boot).
		child.TraceID = newTraceID()
	}
	child.SetAttrs(attrs...)
	if child.bus != nil {
		child.bus.Publish(Event{
			TraceID:   child.TraceID,
			SpanID:    child.SpanID,
			ParentID:  child.ParentID,
			Kind:      KindSpanStart,
			Service:   child.Service,
			Endpoint:  child.Endpoint,
			Name:      child.Name,
			Timestamp: child.Start,
			Meta:      child.snapshotAttrs(),
		})
	}
	return context.WithValue(ctx, spanCtxKey{}, child), child
}

// End closes the span and publishes a span.end event. Safe to call more than
// once — subsequent calls are no-ops. Pass nil for success; pass the handler's
// error for failure so it surfaces on the waterfall.
func (s *Span) End(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.mu.Unlock()
	if s.bus == nil {
		return
	}
	var errStr string
	if err != nil {
		errStr = err.Error()
	}
	s.bus.Publish(Event{
		TraceID:    s.TraceID,
		SpanID:     s.SpanID,
		ParentID:   s.ParentID,
		Kind:       KindSpanEnd,
		Service:    s.Service,
		Endpoint:   s.Endpoint,
		Name:       s.Name,
		DurationMs: time.Since(s.Start).Milliseconds(),
		Error:      errStr,
		Meta:       s.snapshotAttrs(),
	})
}

// In is the common-case sugar for a one-call span. It starts the span and
// returns a closer that ends it when invoked:
//
//	func NewGetUser(...) (u *User, err error) {
//	    defer trace.In(ctx, "db.users.get")(&err)
//	    u, err = db.GetUser(ctx, id)
//	    return
//	}
//
// Pass nil if the caller has no error to report.
func In(ctx context.Context, name string, attrs ...Attr) func(*error) {
	_, span := StartSpan(ctx, name, attrs...)
	return func(errp *error) {
		var err error
		if errp != nil {
			err = *errp
		}
		span.End(err)
	}
}

// --- ID minting (W3C-compatible: 16B trace, 8B span, hex-encoded) ---

func newSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}