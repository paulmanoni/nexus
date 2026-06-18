package app_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"routerseam/app"
	"routerseam/httpx"
	"routerseam/httpx/chirouter"
	"routerseam/httpx/ginrouter"
	"routerseam/httpx/stdrouter"
)

// backends are the three routers, each wired with the SAME app.Register.
func backends() map[string]httpx.Router {
	return map[string]httpx.Router{
		"gin": ginrouter.New(),
		"chi": chirouter.New(),
		"std": stdrouter.New(),
	}
}

type tc struct {
	name       string
	method     string
	path       string
	token      string
	body       string
	wantStatus int
	wantSub    string
}

var cases = []tc{
	{"param+query", "GET", "/pets/42?q=hi", "secret", "", 200, `"id":"42"`},
	{"param echoes query", "GET", "/pets/42?q=hi", "secret", "", 200, `"q":"hi"`},
	{"json create", "POST", "/pets", "secret", `{"name":"rex"}`, 201, `"created":"rex"`},
	{"auth abort", "GET", "/pets/42", "", "", 401, "unauthorized"},
	{"recovery catches panic", "GET", "/boom", "secret", "", 500, "internal"},
	{"spa fallback", "GET", "/totally/unknown/spa/route", "secret", "", 200, "INDEX"},
}

func TestParityAcrossBackends(t *testing.T) {
	for backend, r := range backends() {
		app.Register(r)
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)

		for _, c := range cases {
			t.Run(backend+"/"+c.name, func(t *testing.T) {
				var bodyR *strings.Reader
				if c.body != "" {
					bodyR = strings.NewReader(c.body)
				} else {
					bodyR = strings.NewReader("")
				}
				req, err := http.NewRequest(c.method, srv.URL+c.path, bodyR)
				if err != nil {
					t.Fatal(err)
				}
				if c.token != "" {
					req.Header.Set("X-Token", c.token)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				buf := new(strings.Builder)
				_, _ = copyTo(buf, resp)

				if resp.StatusCode != c.wantStatus {
					t.Errorf("%s: status=%d want %d (body=%q)", backend, resp.StatusCode, c.wantStatus, buf.String())
				}
				if !strings.Contains(buf.String(), c.wantSub) {
					t.Errorf("%s: body=%q want contains %q", backend, buf.String(), c.wantSub)
				}
			})
		}
	}
}

func copyTo(b *strings.Builder, resp *http.Response) (int64, error) {
	buf := make([]byte, 4096)
	var n int64
	for {
		m, err := resp.Body.Read(buf)
		if m > 0 {
			b.Write(buf[:m])
			n += int64(m)
		}
		if err != nil {
			return n, nil
		}
	}
}
