package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// End-to-end benchmarks. The micro-benchmarks in bench_test.go measure
// the cost of inspectHandler / callHandler in isolation; the ones here
// drive a fully booted *App through the public HTTP surface — gin
// routing, the AsRest reflective handler, JSON encode/decode, the
// trace bus, and (for the cross-process variants) loopback TCP.
//
// Read the numbers as: req/s under a single Go process on a single
// machine, with a no-op handler. They are the framework's *ceiling*
// — adding DB / network / business logic only reduces them.
//
// Run:
//
//	go test -bench=BenchmarkE2E -benchmem -run=^$ -benchtime=3s .
//	go test -bench=BenchmarkE2E_REST_InProcess -benchmem -run=^$ -cpuprofile=cpu.out .

func init() {
	// Silence gin's per-request log and route-registration spam during
	// benchmark runs — they would otherwise dominate the cost being
	// measured.
	gin.SetMode(gin.ReleaseMode)
}

// --- handler shapes used below -------------------------------------

type benchEcho struct {
	Message string `json:"message"`
}

type benchEchoArgs struct {
	Q string `json:"q" query:"q" graphql:"q"`
}

type benchCreateArgs struct {
	Title string `json:"title" graphql:"title"`
	Body  string `json:"body"  graphql:"body"`
}

type benchCreateResp struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func benchRestEcho(p Params[benchEchoArgs]) (*benchEcho, error) {
	return &benchEcho{Message: p.Args.Q}, nil
}

func benchGqlEcho(ctx context.Context, a benchEchoArgs) (*benchEcho, error) {
	return &benchEcho{Message: a.Q}, nil
}

var benchCreateSeq uint64

func benchGqlCreate(ctx context.Context, a benchCreateArgs) (*benchCreateResp, error) {
	id := atomic.AddUint64(&benchCreateSeq, 1)
	return &benchCreateResp{ID: itoa(id), Title: a.Title}, nil
}

// itoa avoids strconv.FormatUint allocations showing up in the
// per-op counter — it's the same work either way but keeps the
// hot path obvious.
func itoa(u uint64) string {
	var buf [20]byte
	i := len(buf)
	for u >= 10 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	i--
	buf[i] = byte('0' + u)
	return string(buf[i:])
}

// --- shared setup --------------------------------------------------

// newBenchApp boots a minimal app with the given options, returning
// the running *App and a teardown. Mirrors newApp from path_test.go
// but uses fx.NopLogger and a fixed Config so the cost of repeated
// boots (BenchmarkE2E_Boot) is comparable across runs.
func newBenchApp(b *testing.B, opts ...Option) *testApp {
	b.Helper()
	cfg := Config{
		Server:        ServerConfig{Addr: "127.0.0.1:0"},
		TraceCapacity: 0, // disable trace bus — measure the handler path, not telemetry
	}
	app, err := newApp(cfg, opts...)
	if err != nil {
		b.Fatalf("newApp: %v", err)
	}
	return app
}

// --- 1. REST over real loopback TCP --------------------------------

// BenchmarkE2E_REST_Loopback drives the full network stack: client
// dial, request encode, TCP loopback, gin routing, reflective bind,
// handler, JSON encode, response read. Realistic ceiling for a
// single-process deployment talking to itself.
func BenchmarkE2E_REST_Loopback(b *testing.B) {
	mod := Module("bench_rest",
		AsRest("GET", "/echo", benchRestEcho),
	)
	app := newBenchApp(b, mod)
	defer app.Stop()

	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	url := srv.URL + "/echo?q=hi"
	// srv.Client() defaults to MaxIdleConnsPerHost=2, which starves
	// RunParallel on macOS — workers can't grab an idle conn so they
	// open a fresh ephemeral port every op, exhausting the port pool
	// within seconds. Raise the per-host pool above GOMAXPROCS so each
	// worker keeps a hot conn.
	client := srv.Client()
	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.MaxIdleConnsPerHost = 256
		tr.MaxIdleConns = 256
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(url)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// --- 2. REST in-process (no TCP) -----------------------------------

// BenchmarkE2E_REST_InProcess strips the TCP loopback by calling
// engine.ServeHTTP directly with httptest.NewRecorder. The delta vs.
// BenchmarkE2E_REST_Loopback is the cost of the OS network stack;
// what remains is gin + nexus.
func BenchmarkE2E_REST_InProcess(b *testing.B) {
	mod := Module("bench_rest_inproc",
		AsRest("GET", "/echo", benchRestEcho),
	)
	app := newBenchApp(b, mod)
	defer app.Stop()
	engine := app.Engine()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req, _ := http.NewRequest("GET", "/echo?q=hi", nil)
		for pb.Next() {
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}
	})
}

// --- 3. GraphQL query (no args) ------------------------------------

// BenchmarkE2E_GraphQL_Query measures the query path: graphql-go parse,
// validate, execute, resolver dispatch into the nexus reflective
// handler, JSON encode the envelope.
//
// Uses default Config, which enables the document cache (cap=1024).
// To compare the cost without caching, see BenchmarkE2E_GraphQL_Query_NoCache.
func BenchmarkE2E_GraphQL_Query(b *testing.B) {
	mod := Module("bench_gql",
		AsQuery(benchGqlEcho),
	)
	app := newBenchApp(b, mod)
	defer app.Stop()
	engine := app.Engine()

	body := []byte(`{"query":"{ benchGqlEcho(q:\"hi\") { message } }"}`)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", "/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}
	})
}

// BenchmarkE2E_GraphQL_Query_NoCache is the same benchmark with the
// document cache explicitly disabled, so the parse + validate cost
// is paid on every request. Compare ns/op and allocs against
// BenchmarkE2E_GraphQL_Query to see what the cache is buying.
func BenchmarkE2E_GraphQL_Query_NoCache(b *testing.B) {
	mod := Module("bench_gql_nocache",
		AsQuery(benchGqlEcho),
	)
	// DocumentCacheSize: -1 disables; 0 would mean "default 1024".
	cfg := Config{
		Server:        ServerConfig{Addr: "127.0.0.1:0"},
		TraceCapacity: 0,
		GraphQL:       GraphQLConfig{DocumentCacheSize: -1},
	}
	app, err := newApp(cfg, mod)
	if err != nil {
		b.Fatalf("newApp: %v", err)
	}
	defer app.Stop()
	engine := app.Engine()

	body := []byte(`{"query":"{ benchGqlEcho(q:\"hi\") { message } }"}`)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", "/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}
	})
}

// --- 4. GraphQL mutation (input-object args) -----------------------

// BenchmarkE2E_GraphQL_Mutation measures the mutation path including
// applyArgsFromStruct's input-object binding for multi-field args.
func BenchmarkE2E_GraphQL_Mutation(b *testing.B) {
	mod := Module("bench_gql_mut",
		AsQuery(benchGqlEcho), // graphql-go requires at least one Query field
		AsMutation(benchGqlCreate),
	)
	app := newBenchApp(b, mod)
	defer app.Stop()
	engine := app.Engine()

	body := []byte(`{"query":"mutation { benchGqlCreate(title:\"t\", body:\"b\") { id title } }"}`)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", "/graphql", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}
	})
}

// --- 5. CRUD memory-store path -------------------------------------

type benchNote struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// BenchmarkE2E_CRUD_Read drives the AsCRUD-generated GET /benchnotes/:id
// path after seeding one row. This is the "read by id" hot path most
// apps actually serve: AsRest mount + URI bind + Store.Read + JSON.
func BenchmarkE2E_CRUD_Read(b *testing.B) {
	mod := Module("bench_crud",
		Provide(func(app *App) *Service { return app.Service("notes-bench") }),
		AsCRUD[benchNote](MemoryResolver[benchNote](nil, nil)),
	)
	app := newBenchApp(b, mod)
	defer app.Stop()
	engine := app.Engine()

	// Seed one note so reads have a target.
	seedReq, _ := http.NewRequest("POST", "/benchnotes",
		strings.NewReader(`{"title":"t","body":"b"}`))
	seedReq.Header.Set("Content-Type", "application/json")
	seedRec := httptest.NewRecorder()
	engine.ServeHTTP(seedRec, seedReq)
	if seedRec.Code >= 300 {
		b.Fatalf("seed failed: %d %s", seedRec.Code, seedRec.Body.String())
	}
	var seeded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(seedRec.Body.Bytes(), &seeded); err != nil {
		b.Fatalf("seed decode: %v (body=%s)", err, seedRec.Body.String())
	}
	readPath := "/benchnotes/" + seeded.ID

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req, _ := http.NewRequest("GET", readPath, nil)
		for pb.Next() {
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		}
	})
}

// --- 6. Boot cost --------------------------------------------------

// BenchmarkE2E_Boot measures fx wiring + autoMountGraphQL + route
// installation for a one-endpoint app. Not a per-request number, but
// useful when N modules in a real app push startup past a second.
func BenchmarkE2E_Boot(b *testing.B) {
	mod := Module("bench_boot",
		AsRest("GET", "/echo", benchRestEcho),
		AsQuery(benchGqlEcho),
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app, err := newApp(Config{
			Server:        ServerConfig{Addr: "127.0.0.1:0"},
			TraceCapacity: 0,
		}, mod)
		if err != nil {
			b.Fatal(err)
		}
		app.Stop()
	}
}
