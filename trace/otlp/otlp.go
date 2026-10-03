// Package otlp exports nexus traces to an OpenTelemetry collector over
// OTLP/HTTP (JSON). It reads the same trace bus the dashboard does, so
// every request, live event and child span nexus records is exported,
// without linking the OpenTelemetry SDK.
//
// Turn it on in nexus.toml:
//
//	[runtime.telemetry]
//	otlp_endpoint = "http://localhost:4318"   # spans POST to /v1/traces
//	service_name  = "orders"                  # default: the dashboard name
//	[runtime.telemetry.otlp_headers]
//	authorization = "Bearer ${OTLP_TOKEN}"
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2/trace"
)

// Config configures an Exporter.
type Config struct {
	// Endpoint is the collector's OTLP/HTTP base URL; spans are posted to
	// Endpoint + "/v1/traces" (an Endpoint already ending in /v1/traces is
	// used as is).
	Endpoint string
	// Headers are sent with every export (authentication, tenant).
	Headers map[string]string
	// ServiceName is the service.name resource attribute.
	ServiceName string
	// BatchSize is the most spans one export carries (default 512);
	// Interval is the longest a span waits to be exported (default 2s).
	BatchSize int
	Interval  time.Duration
	// Client posts the exports (default: a client with a 10s timeout).
	Client *http.Client
}

// Exporter batches finished spans from a trace bus and posts them.
type Exporter struct {
	cfg    Config
	url    string
	events <-chan trace.Event
	cancel func()
	done   chan struct{}

	mu      sync.Mutex
	pending []span
	starts  map[string]trace.Event // span ID → its start event, until it ends
	lastErr error
}

// Start subscribes to bus and exports until Stop.
func Start(bus *trace.Bus, cfg Config) *Exporter {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 2 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "nexus"
	}
	url := strings.TrimRight(cfg.Endpoint, "/")
	if !strings.HasSuffix(url, "/v1/traces") {
		url += "/v1/traces"
	}
	_, events, cancel := bus.Subscribe(math.MaxInt64, 4096) // spans from now on
	e := &Exporter{cfg: cfg, url: url, events: events, cancel: cancel, done: make(chan struct{}), starts: map[string]trace.Event{}}
	go e.run()
	return e
}

// Stop flushes what is pending and stops exporting.
func (e *Exporter) Stop(ctx context.Context) error {
	e.cancel()
	select {
	case <-e.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.flush(ctx)
}

// Err is the last export's error, nil once an export succeeds.
func (e *Exporter) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

func (e *Exporter) run() {
	defer close(e.done)
	tick := time.NewTicker(e.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-e.events:
			if !ok {
				return
			}
			if e.add(ev) >= e.cfg.BatchSize {
				_ = e.flush(context.Background())
			}
		case <-tick.C:
			_ = e.flush(context.Background())
		}
	}
}

// add records a start event and turns an end event into a span; it
// returns how many spans are pending.
func (e *Exporter) add(ev trace.Event) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch ev.Kind {
	case trace.KindRequestStart, trace.KindSpanStart:
		if len(e.starts) < 10000 { // spans that never end can't grow it without bound
			e.starts[ev.SpanID] = ev
		}
	case trace.KindRequestEnd, trace.KindSpanEnd:
		start := e.starts[ev.SpanID]
		delete(e.starts, ev.SpanID)
		e.pending = append(e.pending, toSpan(start, ev))
	}
	return len(e.pending)
}

func (e *Exporter) flush(ctx context.Context) error {
	e.mu.Lock()
	batch := e.pending
	e.pending = nil
	e.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	body, err := json.Marshal(request{ResourceSpans: []resourceSpans{{
		Resource:   resource{Attributes: []attr{strAttr("service.name", e.cfg.ServiceName)}},
		ScopeSpans: []scopeSpans{{Scope: scope{Name: "nexus"}, Spans: batch}},
	}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	res, err := e.cfg.Client.Do(req)
	if err == nil {
		res.Body.Close()
		if res.StatusCode >= 300 {
			err = fmt.Errorf("otlp: %s answered %s", e.url, res.Status)
		}
	}
	e.mu.Lock()
	e.lastErr = err
	e.mu.Unlock()
	return err
}

// OTLP/HTTP JSON (the protobuf JSON mapping: 64-bit integers as strings,
// IDs as hex).
type (
	request struct {
		ResourceSpans []resourceSpans `json:"resourceSpans"`
	}
	resourceSpans struct {
		Resource   resource     `json:"resource"`
		ScopeSpans []scopeSpans `json:"scopeSpans"`
	}
	resource struct {
		Attributes []attr `json:"attributes"`
	}
	scopeSpans struct {
		Scope scope  `json:"scope"`
		Spans []span `json:"spans"`
	}
	scope struct {
		Name string `json:"name"`
	}
	span struct {
		TraceID      string `json:"traceId"`
		SpanID       string `json:"spanId"`
		ParentSpanID string `json:"parentSpanId,omitempty"`
		Name         string `json:"name"`
		Kind         int    `json:"kind"`
		Start        string `json:"startTimeUnixNano"`
		End          string `json:"endTimeUnixNano"`
		Attributes   []attr `json:"attributes,omitempty"`
		Status       status `json:"status"`
	}
	status struct {
		Code    int    `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	}
	attr struct {
		Key   string `json:"key"`
		Value value  `json:"value"`
	}
	value struct {
		String *string  `json:"stringValue,omitempty"`
		Int    *string  `json:"intValue,omitempty"`
		Double *float64 `json:"doubleValue,omitempty"`
		Bool   *bool    `json:"boolValue,omitempty"`
	}
)

const (
	kindInternal = 1
	kindServer   = 2
	statusError  = 2
)

func toSpan(start, end trace.Event) span {
	endAt := end.Timestamp
	startAt := start.Timestamp
	if startAt.IsZero() {
		startAt = endAt.Add(-time.Duration(end.DurationMs) * time.Millisecond)
	}
	name := first(end.Name, start.Name, end.Endpoint, strings.TrimSpace(end.Method+" "+end.Path))
	s := span{
		TraceID:      end.TraceID,
		SpanID:       end.SpanID,
		ParentSpanID: end.ParentID,
		Name:         name,
		Kind:         kindInternal,
		Start:        strconv.FormatInt(startAt.UnixNano(), 10),
		End:          strconv.FormatInt(endAt.UnixNano(), 10),
	}
	if end.Kind == trace.KindRequestEnd {
		s.Kind = kindServer
	}
	add := func(a attr) { s.Attributes = append(s.Attributes, a) }
	if end.Method != "" {
		add(strAttr("http.request.method", end.Method))
	}
	if end.Path != "" {
		add(strAttr("url.path", end.Path))
	}
	if end.Status != 0 {
		add(intAttr("http.response.status_code", int64(end.Status)))
	}
	for k, v := range map[string]string{"nexus.service": end.Service, "nexus.endpoint": end.Endpoint, "nexus.transport": end.Transport} {
		if v != "" {
			add(strAttr(k, v))
		}
	}
	meta := map[string]any{}
	for k, v := range start.Meta {
		meta[k] = v
	}
	for k, v := range end.Meta {
		meta[k] = v
	}
	for k, v := range meta {
		if a, ok := anyAttr(k, v); ok {
			add(a)
		}
	}
	if end.Error != "" || end.Status >= 500 {
		s.Status = status{Code: statusError, Message: end.Error}
	}
	return s
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return "span"
}

func strAttr(k, v string) attr { return attr{Key: k, Value: value{String: &v}} }
func intAttr(k string, v int64) attr {
	s := strconv.FormatInt(v, 10)
	return attr{Key: k, Value: value{Int: &s}}
}

func anyAttr(k string, v any) (attr, bool) {
	switch v := v.(type) {
	case string:
		return strAttr(k, v), true
	case bool:
		return attr{Key: k, Value: value{Bool: &v}}, true
	case int:
		return intAttr(k, int64(v)), true
	case int64:
		return intAttr(k, v), true
	case int32:
		return intAttr(k, int64(v)), true
	case float64:
		return attr{Key: k, Value: value{Double: &v}}, true
	case float32:
		f := float64(v)
		return attr{Key: k, Value: value{Double: &f}}, true
	case fmt.Stringer:
		return strAttr(k, v.String()), true
	case nil:
		return attr{}, false
	}
	return strAttr(k, fmt.Sprint(v)), true
}
