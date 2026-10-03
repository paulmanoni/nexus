package otlp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2/trace"
)

// Finished spans reach the collector as OTLP/HTTP JSON: a server span for
// the request, its child under it, attributes and an error status.
func TestExportsFinishedSpans(t *testing.T) {
	var mu sync.Mutex
	var got []request
	var auth string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var req request
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("body: %v", err)
		}
		mu.Lock()
		got = append(got, req)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
	}))
	defer collector.Close()

	bus := trace.NewBus(256)
	exp := Start(bus, Config{Endpoint: collector.URL, ServiceName: "orders", Headers: map[string]string{"Authorization": "Bearer t"}, Interval: time.Hour})

	ctx := trace.WithBus(context.Background(), bus)
	ctx, root, finish := trace.NewRootSpan(ctx, "orders.Create", "orders", "orders.Create", "rest", trace.Str("order.kind", "gift"))
	_, child := trace.StartSpan(ctx, "db.insert")
	child.Set("rows", 1)
	child.End(nil)
	root.Set("bytes", 42)
	finish(500, errString("boom"))

	time.Sleep(50 * time.Millisecond) // let the exporter take the events
	if err := exp.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || auth != "Bearer t" {
		t.Fatalf("exports = %d, auth = %q", len(got), auth)
	}
	rs := got[0].ResourceSpans[0]
	if v := rs.Resource.Attributes[0]; v.Key != "service.name" || *v.Value.String != "orders" {
		t.Fatalf("resource = %+v", rs.Resource)
	}
	spans := map[string]span{}
	for _, s := range rs.ScopeSpans[0].Spans {
		spans[s.Name] = s
	}
	r, c := spans["orders.Create"], spans["db.insert"]
	if r.Kind != kindServer || r.Status.Code != statusError || r.Status.Message != "boom" {
		t.Errorf("root = %+v", r)
	}
	if c.ParentSpanID != r.SpanID || c.TraceID != r.TraceID || c.Kind != kindInternal {
		t.Errorf("child = %+v, root = %+v", c, r)
	}
	if !hasAttr(r, "order.kind") || !hasAttr(r, "bytes") || !hasAttr(c, "rows") {
		t.Errorf("attributes: root %+v child %+v", r.Attributes, c.Attributes)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func hasAttr(s span, key string) bool {
	for _, a := range s.Attributes {
		if a.Key == key {
			return true
		}
	}
	return false
}
