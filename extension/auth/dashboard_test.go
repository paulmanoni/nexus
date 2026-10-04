package auth_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

func TestDashboardSessionsLookup(t *testing.T) {
	s := testSettings
	app, stop, err := nexus.InProcess(config.Runtime{Introspection: true, Dashboard: config.Dashboard{Enabled: true}},
		auth.Module(auth.Config{Users: auth.StaticUsers(newStage4Users()), Settings: &s}),
		orderPolicy,
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("GET", "/me", whoAmI),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	srv := httptest.NewServer(app)
	defer srv.Close()

	phone := newBrowser(t, srv)
	phone.header["User-Agent"] = "phone/9"
	phone.do("GET", "/health", "")
	phone.do("POST", "/login", `{"login":"ana","password":"correct horse"}`)

	admin := newBrowser(t, srv)
	_, page := admin.do("GET", "/__nexus/ui/auth?user=1", "")
	for _, want := range []string{"phone/9", "/auth/revoke-session", "*auth_test.order"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Auth page lacks %q", want)
		}
	}
	i := strings.Index(page, "&#34;id&#34;:&#34;")
	if i < 0 {
		t.Fatal("no revoke button")
	}
	id := page[i+len("&#34;id&#34;:&#34;") : i+len("&#34;id&#34;:&#34;")+32]
	admin.do("GET", "/health", "")
	if code, body := admin.do("POST", "/__nexus/auth/revoke-session", `{"user":"1","id":"`+id+`"}`); code != 200 {
		t.Fatalf("revoke-session = %d %s", code, body)
	}
	if code, _ := meCode(phone); code != 401 {
		t.Fatalf("the revoked session = %d", code)
	}
}
