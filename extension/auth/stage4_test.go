package auth_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
)

func newStage4Users() *testUsers {
	u := newTestUsers()
	bc := auth.BCryptCost(4)
	h, _ := bc.Hash("admin pass")
	u.byID["4"] = &account{id: "4", login: "root", hash: h, kind: "staff", perms: []string{"*"}}
	m, _ := bc.Hash("manager pass")
	u.byID["5"] = &account{id: "5", login: "mia", hash: m, kind: "staff", perms: []string{"auth.impersonate", "orders.view"}}
	return u
}

type actorOut struct {
	ID    string `json:"id"`
	Actor string `json:"actor"`
}

func whoActs(ctx context.Context) (*actorOut, error) {
	me := auth.Current(ctx)
	out := &actorOut{ID: me.ID}
	if me.Actor != nil {
		out.Actor = me.Actor.ID
	}
	return out, nil
}

type order struct{ Owner string }

var orderPolicy = auth.Policy(func(ctx context.Context, me *auth.Identity, perm string, o *order) bool {
	return o.Owner == me.ID || me.Has("orders.*")
})

type orderIn struct {
	Owner string `query:"owner"`
}

func viewOrder(ctx context.Context, in orderIn) (*me, error) {
	if err := auth.Check(ctx, "", &order{Owner: in.Owner}); err != nil {
		return nil, err
	}
	return &me{ID: in.Owner}, nil
}

var (
	ranAsMu sync.Mutex
	ranAs   []string
)

var recordJob = jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, _ struct{}) error {
	ranAsMu.Lock()
	defer ranAsMu.Unlock()
	if me := auth.Current(ctx); me != nil {
		ranAs = append(ranAs, me.ID+":"+me.Kind)
	}
	return nil
}, jobs.Name("auth-test-record"))

var systemJob = jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, _ struct{}) error {
	ranAsMu.Lock()
	defer ranAsMu.Unlock()
	who := "system"
	if me := auth.Current(ctx); me != nil {
		who = me.ID
	}
	ranAs = append(ranAs, "sys:"+who)
	return nil
}, jobs.Name("auth-test-system"), jobs.AsSystem())

func enqueue(ctx context.Context) (*me, error) {
	_, err := recordJob.Enqueue(ctx, struct{}{})
	if err == nil {
		_, err = systemJob.Enqueue(ctx, struct{}{})
	}
	return &me{}, err
}

func stage4App(t *testing.T) *httptest.Server {
	t.Helper()
	s := testSettings
	s.Impersonation = auth.ImpersonationSettings{Endpoint: "/impersonate"}
	s.Endpoints = auth.EndpointSettings{Me: "/auth/me"}
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newStage4Users()), Settings: &s}),
		jobs.Module(jobs.Config{}), recordJob, systemJob, orderPolicy,
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("GET", "/who", whoActs),
		nexus.AsRest("GET", "/order", viewOrder),
		nexus.AsRest("POST", "/enqueue", enqueue),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func signedInAs(t *testing.T, srv *httptest.Server, login, pw, scheme string) *browser {
	t.Helper()
	b := newBrowser(t, srv)
	b.do("GET", "/health", "")
	code, body := b.do("POST", "/login", `{"login":"`+login+`","password":"`+pw+`","scheme":"`+scheme+`"}`)
	if code != 201 {
		t.Fatalf("sign-in %s = %d %s", login, code, body)
	}
	if scheme != "" {
		var c auth.Credential
		_ = json.Unmarshal([]byte(body), &c)
		b.bearer = c.AccessToken
	}
	return b
}

func who(t *testing.T, b *browser) actorOut {
	t.Helper()
	code, body := b.do("GET", "/who", "")
	var a actorOut
	if code != 200 || json.Unmarshal([]byte(body), &a) != nil {
		t.Fatalf("/who = %d %s", code, body)
	}
	return a
}

func TestImpersonation(t *testing.T) {
	srv := stage4App(t)
	for _, scheme := range []string{"", "api"} {
		root := signedInAs(t, srv, "root", "admin pass", scheme)
		if code, body := root.do("POST", "/impersonate", `{"user_id":"1"}`); code != 201 {
			t.Fatalf("[%s] impersonate = %d %s", scheme, code, body)
		}
		if a := who(t, root); a.ID != "1" || a.Actor != "4" {
			t.Fatalf("[%s] while impersonating: %+v", scheme, a)
		}
		_, body := root.do("GET", "/auth/me", "")
		if !strings.Contains(body, `"actor":{"id":"4"`) {
			t.Fatalf("[%s] me shows the actor: %s", scheme, body)
		}
		if code, _ := root.do("POST", "/impersonate", `{"user_id":"2"}`); code != 409 {
			t.Fatalf("[%s] nested impersonation = %d, want 409", scheme, code)
		}
		if code, _ := root.do("DELETE", "/impersonate", ""); code != 200 {
			t.Fatalf("[%s] stop = %d", scheme, code)
		}
		if a := who(t, root); a.ID != "4" || a.Actor != "" {
			t.Fatalf("[%s] after stopping: %+v", scheme, a)
		}
	}

	ana := signedInAs(t, srv, "ana", "correct horse", "")
	if code, _ := ana.do("POST", "/impersonate", `{"user_id":"2"}`); code != 403 {
		t.Fatalf("without the permission = %d, want 403", code)
	}
	mia := signedInAs(t, srv, "mia", "manager pass", "")
	if code, body := mia.do("POST", "/impersonate", `{"user_id":"1"}`); code != 403 || !strings.Contains(body, "orders.*") {
		t.Fatalf("escalating to orders.* = %d %s", code, body)
	}
	if code, _ := mia.do("POST", "/impersonate", `{"user_id":"2"}`); code != 201 {
		t.Fatalf("impersonating a user with fewer permissions = %d", code)
	}
	if code, _ := mia.do("POST", "/impersonate", `{"user_id":"404"}`); code != 409 {
		t.Fatalf("while impersonating, another start = %d", code)
	}
}

func TestPolicies(t *testing.T) {
	srv := stage4App(t)
	bo := signedInAs(t, srv, "bo", "old format pw", "")
	if code, _ := bo.do("GET", "/order?owner=2", ""); code != 200 {
		t.Fatalf("an owner's own order = %d", code)
	}
	if code, _ := bo.do("GET", "/order?owner=1", ""); code != 403 {
		t.Fatalf("another's order = %d, want 403", code)
	}
	ana := signedInAs(t, srv, "ana", "correct horse", "")
	if code, _ := ana.do("GET", "/order?owner=2", ""); code != 200 {
		t.Fatalf("orders.* sees every order = %d", code)
	}
}

func TestJobsRunAsTheirEnqueuer(t *testing.T) {
	srv := stage4App(t)
	ana := signedInAs(t, srv, "ana", "correct horse", "")
	if code, body := ana.do("POST", "/enqueue", ""); code != 201 {
		t.Fatalf("enqueue = %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ranAsMu.Lock()
		got := strings.Join(ranAs, ",")
		ranAsMu.Unlock()
		if strings.Contains(got, "1:staff") && strings.Contains(got, "sys:system") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	ranAsMu.Lock()
	t.Logf("ran: %v", ranAs)
	ranAsMu.Unlock()
	t.Fatal("the job did not run")
}
