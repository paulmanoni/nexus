package nexus

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownTimeoutResolution(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	if got := shutdownTimeout(Config{}); got != DefaultShutdownTimeout {
		t.Fatalf("production default = %s, want %s", got, DefaultShutdownTimeout)
	}
	cfg := Config{Server: ServerConfig{ShutdownTimeout: 2 * time.Second}}
	if got := shutdownTimeout(cfg); got != 2*time.Second {
		t.Fatalf("explicit config = %s, want 2s", got)
	}

	t.Setenv("NEXUS_DEV", "1")
	if got := shutdownTimeout(Config{}); got != DevShutdownTimeout {
		t.Fatalf("dev default = %s, want %s", got, DevShutdownTimeout)
	}
	// Explicit config still wins in dev — an operator who asked for a drain
	// gets one wherever they're running.
	if got := shutdownTimeout(cfg); got != 2*time.Second {
		t.Fatalf("explicit config in dev = %s, want 2s", got)
	}
}

func TestShutdownTimeoutFromTOML(t *testing.T) {
	cfg, err := configFromTOML([]byte("[runtime.server]\naddr = \":9999\"\nshutdown_timeout = \"3s\"\n"), "test")
	if err != nil {
		t.Fatalf("configFromTOML: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 3*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 3s", cfg.Server.ShutdownTimeout)
	}
	// A malformed duration degrades to the default rather than refusing to
	// boot — shutdown timing is a tuning knob, not a correctness one.
	cfg, err = configFromTOML([]byte("[runtime.server]\nshutdown_timeout = \"soon\"\n"), "test")
	if err != nil {
		t.Fatalf("configFromTOML with a bad duration: %v", err)
	}
	if cfg.Server.ShutdownTimeout != 0 {
		t.Fatalf("bad duration produced %s, want 0 (fall through to default)", cfg.Server.ShutdownTimeout)
	}
}

// The regression this whole change exists for: a request still in flight must
// not hold shutdown open for the full grace window. The handler selects on its
// request context, so cancelling the server's BaseContext lets it return and
// Shutdown completes immediately.
func TestShutdownCancelsInFlightRequests(t *testing.T) {
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	entered := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		BaseContext:       func(l net.Listener) context.Context { return reqCtx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done() // hangs until someone cancels us
			w.WriteHeader(http.StatusOK)
		}),
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered

	start := time.Now()
	cancelReqs()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Shutdown took %s with one in-flight request; the cancel didn't reach the handler", el)
	}
}
