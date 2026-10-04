package auth_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
)

type devicesOut struct {
	Devices []auth.Device `json:"devices"`
}

func listSessions(ctx context.Context) (*devicesOut, error) {
	d, err := auth.Sessions(ctx, auth.Current(ctx).ID)
	return &devicesOut{d}, err
}

type idIn struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func revokeDevice(ctx context.Context, in idIn) (*me, error) {
	return &me{}, auth.RevokeSession(ctx, auth.Current(ctx).ID, in.ID)
}

type keyOut struct {
	Key    string      `json:"key"`
	Device auth.Device `json:"device"`
}

func createKey(ctx context.Context, in idIn) (*keyOut, error) {
	k, d, err := auth.Keys.Create(ctx, auth.Current(ctx).ID, "keys", in.Name)
	if err != nil {
		return nil, err
	}
	return &keyOut{k, *d}, nil
}

func listKeys(ctx context.Context) (*devicesOut, error) {
	d, err := auth.Keys.List(ctx, auth.Current(ctx).ID)
	return &devicesOut{d}, err
}

func revokeKey(ctx context.Context, in idIn) (*me, error) {
	return &me{}, auth.Keys.Revoke(ctx, auth.Current(ctx).ID, in.ID)
}

func devicesApp(t *testing.T, tokens auth.TokenStore) *httptest.Server {
	t.Helper()
	s := testSettings
	app, stop, err := nexus.InProcess(config.Runtime{},
		auth.Module(auth.Config{Users: auth.StaticUsers(newTestUsers()), Settings: &s, Tokens: tokens}),
		nexus.AsRest("POST", "/login", signIn, auth.Public()),
		nexus.AsRest("GET", "/devices", listSessions),
		nexus.AsRest("POST", "/devices/revoke", revokeDevice),
		nexus.AsRest("POST", "/keys", createKey),
		nexus.AsRest("GET", "/keys", listKeys),
		nexus.AsRest("POST", "/keys/revoke", revokeKey),
		nexus.AsRest("POST", "/revoke-me", revokeMe),
		nexus.AsRest("GET", "/me", whoAmI),
		nexus.AsRest("GET", "/health", health, auth.Public()),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func devices(t *testing.T, b *browser, path string) []auth.Device {
	t.Helper()
	code, body := b.do("GET", path, "")
	var out devicesOut
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	return out.Devices
}

func TestSessionsAndKeys(t *testing.T) {
	for name, store := range map[string]auth.TokenStore{
		"memory": auth.NewMemoryTokenStore(),
		"cache":  auth.CacheTokens(&fakeCache{m: map[string][]byte{}}),
	} {
		t.Run(name, func(t *testing.T) {
			srv := devicesApp(t, store)
			phone := newBrowser(t, srv)
			phone.header["User-Agent"] = "phone/1"
			phone.do("GET", "/health", "")
			phone.do("POST", "/login", `{"login":"ana","password":"correct horse"}`)
			laptop := signedIn(t, srv, "")
			api := signedIn(t, srv, "api")

			list := devices(t, laptop, "/devices")
			kinds, current, phoneID := map[string]int{}, 0, ""
			for _, d := range list {
				kinds[d.Kind]++
				if d.Current {
					current++
				}
				if d.Agent == "phone/1" {
					phoneID = d.ID
				}
			}
			if len(list) != 3 || kinds["session"] != 2 || kinds["token"] != 1 || current != 1 || phoneID == "" {
				t.Fatalf("Sessions = %+v", list)
			}

			if code, _ := laptop.do("POST", "/devices/revoke", `{"id":"`+phoneID+`"}`); code != 201 {
				t.Fatalf("RevokeSession = %d", code)
			}
			if code, body := meCode(phone); code != 401 {
				t.Fatalf("the revoked session = %d %s", code, body)
			}
			if laptop.me().ID != "1" || api.me().ID != "1" {
				t.Fatal("the other sessions stay")
			}
			if n := len(devices(t, laptop, "/devices")); n != 2 {
				t.Fatalf("after RevokeSession: %d devices", n)
			}
			if code, _ := laptop.do("POST", "/devices/revoke", `{"id":"nope"}`); code != 404 {
				t.Fatalf("an unknown device = %d", code)
			}

			code, body := laptop.do("POST", "/keys", `{"name":"billing sync"}`)
			var k keyOut
			_ = json.Unmarshal([]byte(body), &k)
			if code != 201 || k.Key == "" || k.Device.Name != "billing sync" || k.Device.Kind != "key" {
				t.Fatalf("Keys.Create = %d %s", code, body)
			}
			withKey := newBrowser(t, srv)
			withKey.header["X-Key"] = k.Key
			if withKey.me().ID != "1" {
				t.Fatal("the key works")
			}
			if ks := devices(t, laptop, "/keys"); len(ks) != 1 || ks[0].Name != "billing sync" {
				t.Fatalf("Keys.List = %+v", ks)
			}
			if n := len(devices(t, laptop, "/devices")); n != 2 {
				t.Fatal("a key isn't listed as a session")
			}
			laptop.do("POST", "/keys/revoke", `{"id":"`+k.Device.ID+`"}`)
			if code, _ := meCode(withKey); code != 401 {
				t.Fatal("a revoked key stops working")
			}

			laptop.do("POST", "/revoke-me", "")
			fresh := signedIn(t, srv, "")
			if list := devices(t, fresh, "/devices"); len(list) != 1 || !list[0].Current {
				t.Fatalf("after RevokeUser only the new session is listed: %+v", list)
			}
		})
	}
}

type plainStore struct{ auth.TokenStore }

func TestSessionsNeedALister(t *testing.T) {
	srv := devicesApp(t, plainStore{auth.NewMemoryTokenStore()})
	b := signedIn(t, srv, "")
	if code, body := b.do("GET", "/devices", ""); code != 500 {
		t.Fatalf("a store without ListUser = %d %s", code, body)
	}
}
