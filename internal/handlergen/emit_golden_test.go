package handlergen

import (
	"testing"

	"github.com/paulmanoni/nexus/internal/golden"
)

// TestEmit_Golden snapshots the FULL generated _gen.go for a broad annotation
// catalog: every primary keyword (provide/rest/query/mutation/subscription/ws/
// worker), auth + use modifiers (with collected imports), and a custom qualified
// //@pkg.Func decorator with and without a trailing modifier. The exact-string
// tests in emit_test.go cover individual branches; this pins the whole emitted
// file (header, import block ordering, gofmt result) so any drift is a diff.
// Regenerate with: UPDATE_GOLDEN=1 go test ./internal/handlergen/...
func TestEmit_Golden(t *testing.T) {
	anns := []Annotation{
		{Func: "NewUsersService", Keyword: "provide", Line: 10},
		{Func: "NewGetUser", Keyword: "rest", Args: []string{"GET", "/users/:id"}, Line: 20},
		{Func: "NewListUsers", Keyword: "query", Line: 30},
		{Func: "NewCreateUser", Keyword: "mutation", Line: 40},
		{Func: "NewCreateUser", Keyword: "auth", Args: []string{`Requires("ADMIN")`}, Line: 41},
		{Func: "NewUserEvents", Keyword: "subscription", Line: 50},
		{Func: "NewChatSend", Keyword: "ws", Args: []string{"/events", "chat.send"}, Line: 60},
		{Func: "NewChatSend", Keyword: "auth", Args: []string{"Required"}, Line: 61},
		{Func: "NewThrottled", Keyword: "rest", Args: []string{"POST", "/throttled"}, Line: 70},
		{Func: "NewThrottled", Keyword: "use", Args: []string{"ratelimit.Per(time.Minute,", "60)"}, Line: 71,
			Imports: []string{`"github.com/paulmanoni/nexus/extension/ratelimit"`, `"time"`}},
		{Func: "NewReaper", Keyword: "worker", Args: []string{"reaper"}, Line: 80},
		{Func: "NewDashboard", Keyword: "inertia.Page", Args: []string{`"GET"`, `"/dash"`, `"Dashboard"`}, Line: 90,
			Imports: []string{`"github.com/paulmanoni/nexus/extension/inertia"`}},
		{Func: "NewDashboard", Keyword: "auth", Args: []string{"Required"}, Line: 91},
	}

	got, err := Emit(Config{Package: "handlers"}, anns)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	golden.Assert(t, got, "full_catalog_gen.go")
}
