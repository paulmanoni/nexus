package nexus

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/gql"
)

type seamKey struct{}

type seamService struct{ *Service }

type seamItem struct {
	Name string `json:"name"`
}

type seamStatus string

type seamFilter struct {
	Prefix string `graphql:"prefix"`
}

type seamArgs struct {
	Name   string     `graphql:"name,required"`
	Status seamStatus `graphql:"status,type=SeamStatus"`
	Filter seamFilter `graphql:"filter,type=SeamFilter"`
}

func gqlDo(t *testing.T, app *App, query string) string {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/graphql", strings.NewReader(query))
	r.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, r)
	return w.Body.String()
}

func TestGraphQLSeam(t *testing.T) {
	RegisterGqlType("SeamStatus", seamStatus("active"), seamStatus("archived"))
	RegisterGqlType[seamFilter]("SeamFilter")

	var (
		seenInfo gql.Info
		seenMw   gql.Field
	)
	handler := func(s *seamService, p Params[seamArgs]) (*seamItem, error) {
		seenInfo = p.Info
		tag, _ := p.Context.Value(seamKey{}).(string)
		return &seamItem{Name: p.Args.Name + "/" + string(p.Args.Status) + "/" + p.Args.Filter.Prefix + "/" + tag}, nil
	}
	tagCtx := func(next gql.Resolver) gql.Resolver {
		return func(f gql.Field) (any, error) {
			seenMw = f
			f.Context = context.WithValue(f.Context, seamKey{}, "mw")
			f.Args["name"] = strings.ToUpper(f.Args["name"].(string))
			return next(f)
		}
	}
	app, stop, err := InProcess(config.Runtime{},
		Provide(func(app *App) *seamService { return &seamService{app.Service("seam")} }),
		AsQuery(handler, Op("seamItem"), GraphMiddleware("tag", "adds a context value", tagCtx)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	body := gqlDo(t, app, `{"query":"query Lookup { seamItem(name: \"rex\", status: archived, filter: {prefix: \"p\"}) { name } }"}`)
	if !strings.Contains(body, `"REX/archived/p/mw"`) {
		t.Fatalf("response = %s", body)
	}
	want := gql.Info{FieldName: "seamItem", ParentType: "Query", Operation: "query", OperationName: "Lookup"}
	if seenInfo != want {
		t.Fatalf("Params.Info = %+v, want %+v", seenInfo, want)
	}
	if seenMw.Info != want || seenMw.Args["status"] != seamStatus("archived") {
		t.Fatalf("middleware saw %+v", seenMw)
	}

	if body := gqlDo(t, app, `{"query":"{ seamItem(name: \"rex\", status: deleted) { name } }"}`); !strings.Contains(body, "errors") {
		t.Fatalf("an unknown enum member must fail: %s", body)
	}
}

func TestRegisterGqlTypeRejectsUnrepresentable(t *testing.T) {
	for name, fn := range map[string]func(){
		"enum of structs": func() { RegisterGqlType("Bad", seamFilter{}) },
		"scalar no enum":  func() { RegisterGqlType[int]("Bad") },
		"empty name":      func() { RegisterGqlType[seamFilter]("") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: want a panic", name)
				}
			}()
			fn()
		}()
	}
}
