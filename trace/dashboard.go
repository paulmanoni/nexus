package trace

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/httpx"
)

// MountDashboard mounts the trace introspection surface onto the
// supplied dashboard router group:
//
//	GET /events       → WebSocket live stream of trace events
//	GET /traces/:id   → reconstructed span tree for one trace
//
// Called by the dashboard package — keeps the handlers in the package
// that owns the Bus.
func MountDashboard(g httpx.Group, bus *Bus) {
	g.GET("/events", streamEvents(bus))
	g.GET("/traces/:id", traceByID(bus))
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// traceSpan is one node in the dashboard waterfall. Times are relative
// to the trace's earliest event so the UI can render bars without
// knowing absolute clock.
type traceSpan struct {
	SpanID     string         `json:"spanId"`
	ParentID   string         `json:"parentId,omitempty"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Service    string         `json:"service,omitempty"`
	Endpoint   string         `json:"endpoint,omitempty"`
	Transport  string         `json:"transport,omitempty"`
	StartMs    int64          `json:"startMs"`
	DurationMs int64          `json:"durationMs"`
	Status     int            `json:"status,omitempty"`
	Error      string         `json:"error,omitempty"`
	Remote     bool           `json:"remote,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// traceByID reconstructs a span tree from the ring buffer. Merges
// request.start / request.end (the root) and span.start / span.end
// (children) into one node per SpanID. Events without a SpanID (e.g.
// KindDownstream markers) are skipped — they'd have no bar to render.
func traceByID(bus *Bus) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		id := c.Param("id")
		events := bus.SnapshotByTrace(id)
		if len(events) == 0 {
			c.JSON(http.StatusNotFound, httpx.H{"error": "trace not found"})
			return
		}
		base := events[0].Timestamp
		for _, e := range events {
			if !e.Timestamp.IsZero() && e.Timestamp.Before(base) {
				base = e.Timestamp
			}
		}
		spans := map[string]*traceSpan{}
		for _, e := range events {
			if e.SpanID == "" {
				continue
			}
			node, ok := spans[e.SpanID]
			if !ok {
				node = &traceSpan{
					SpanID:   e.SpanID,
					ParentID: e.ParentID,
					Service:  e.Service,
					Endpoint: e.Endpoint,
					Remote:   e.Remote,
				}
				spans[e.SpanID] = node
			}
			if e.Name != "" {
				node.Name = e.Name
			}
			if e.Transport != "" {
				node.Transport = e.Transport
			}
			switch e.Kind {
			case KindRequestStart, KindSpanStart:
				node.Kind = string(e.Kind)
				if !e.Timestamp.IsZero() {
					node.StartMs = e.Timestamp.Sub(base).Milliseconds()
				}
				if e.Meta != nil {
					node.Attrs = e.Meta
				}
			case KindRequestEnd, KindSpanEnd:
				node.DurationMs = e.DurationMs
				if e.Error != "" {
					node.Error = e.Error
				}
				if e.Status != 0 {
					node.Status = e.Status
				}
				if e.Meta != nil {
					node.Attrs = e.Meta
				}
			}
			if node.Name == "" {
				node.Name = e.Endpoint
			}
		}
		out := make([]*traceSpan, 0, len(spans))
		for _, s := range spans {
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].StartMs != out[j].StartMs {
				return out[i].StartMs < out[j].StartMs
			}
			return out[i].SpanID < out[j].SpanID
		})
		c.JSON(http.StatusOK, httpx.H{"traceId": id, "spans": out})
	}
}

func streamEvents(bus *Bus) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var since int64
		if s := c.Query("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		backlog, ch, cancel := bus.Subscribe(since, 128)
		defer cancel()

		// Detect client close so a half-open connection (browser tab
		// closed, laptop slept, firewall idle-killed) stops fanning
		// events into a black hole. NextReader blocks until either
		// the peer sends a frame (the dashboard never does) or the
		// conn errors — on the latter, signal the writer loop.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.NextReader(); err != nil {
					return
				}
			}
		}()

		// writeJSON wraps each Write with a deadline so a slow /
		// backgrounded client can't pin the goroutine forever. Without
		// this, WriteJSON blocks indefinitely on TCP backpressure and
		// the dashboard appears "stuck" — bus.Publish keeps dropping
		// for this subscriber but the conn never errors, so the
		// frontend's auto-reconnect never kicks in. The deadline forces
		// the loop to error out and close, the client reconnects with
		// since=lastId and recovers cleanly.
		writeJSON := func(v any) error {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteJSON(v)
		}

		for _, e := range backlog {
			if err := writeJSON(e); err != nil {
				return
			}
		}
		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-closed:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				if err := writeJSON(e); err != nil {
					return
				}
			}
		}
	}
}
