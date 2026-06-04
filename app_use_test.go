package nexus

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
)

func ginOnlyBundle(name string) middleware.Middleware {
	return middleware.Middleware{Name: name, Gin: func(*gin.Context) {}}
}

func graphOnlyBundle(name string) middleware.Middleware {
	return middleware.Middleware{
		Name:  name,
		Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next },
	}
}

func bothBundle(name string) middleware.Middleware {
	return middleware.Middleware{
		Name:  name,
		Gin:   func(*gin.Context) {},
		Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next },
	}
}

func TestCheckBundleTransports(t *testing.T) {
	tests := []struct {
		name    string
		bundles []middleware.Middleware
		on      middleware.Transport
		wantErr bool
	}{
		{"gin bundle on REST passes", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportREST, false},
		{"gin bundle on WS passes", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportWebSocket, false},
		{"gin bundle on GraphQL fails", []middleware.Middleware{ginOnlyBundle("a")}, middleware.TransportGraphQL, true},
		{"graph bundle on GraphQL passes", []middleware.Middleware{graphOnlyBundle("b")}, middleware.TransportGraphQL, false},
		{"graph bundle on REST fails", []middleware.Middleware{graphOnlyBundle("b")}, middleware.TransportREST, true},
		{"both-transport bundle on any passes", []middleware.Middleware{bothBundle("c")}, middleware.TransportGraphQL, false},
		{"empty/metadata bundle is allowed anywhere", []middleware.Middleware{{Name: "label"}}, middleware.TransportGraphQL, false},
		{"no bundles passes", nil, middleware.TransportREST, false},
		{"first bad bundle reported among many", []middleware.Middleware{bothBundle("ok"), ginOnlyBundle("bad")}, middleware.TransportGraphQL, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkBundleTransports(tc.bundles, tc.on, "someOp")
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestCheckBundleTransportsMessage(t *testing.T) {
	err := checkBundleTransports([]middleware.Middleware{ginOnlyBundle("auth:custom")}, middleware.TransportGraphQL, "createAdvert")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"auth:custom", "GraphQL", "createAdvert", "UseOnGraph"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message missing %q: %s", want, err.Error())
		}
	}
}
