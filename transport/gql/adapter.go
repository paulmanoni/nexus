// Package gql mounts a GraphQL schema (typically assembled by
// github.com/paulmanoni/go-graph) onto Gin and introspects its operations
// into the nexus registry. nexus does NOT own schema assembly — the caller
// keeps using go-graph (or graphql-go directly) and hands nexus the finished *graphql.Schema.
package gql

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/graphql-go/graphql"

	"nexus/registry"
	"nexus/trace"
)

// Mount attaches schema at path for POST/GET and auto-registers every
// operation (queries, mutations, subscriptions) into the registry for the dashboard.
// If bus != nil, requests are traced.
func Mount(e *gin.Engine, r *registry.Registry, bus *trace.Bus, service, path string, schema *graphql.Schema) {
	registerOps(r, service, path, schema)
	h := handler(schema)
	var handlers []gin.HandlerFunc
	if bus != nil {
		handlers = append(handlers, trace.Middleware(bus, service, "POST "+path, string(registry.GraphQL)))
	}
	handlers = append(handlers, h)
	e.POST(path, handlers...)
	e.GET(path, handlers...)
}

type request struct {
	Query         string         `json:"query"         form:"query"`
	OperationName string         `json:"operationName" form:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func handler(schema *graphql.Schema) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req request
		if c.Request.Method == http.MethodGet {
			req.Query = c.Query("query")
			req.OperationName = c.Query("operationName")
			if v := c.Query("variables"); v != "" {
				_ = json.Unmarshal([]byte(v), &req.Variables)
			}
		} else {
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		result := graphql.Do(graphql.Params{
			Schema:         *schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        c.Request.Context(),
		})
		c.JSON(http.StatusOK, result)
	}
}

func registerOps(r *registry.Registry, service, path string, schema *graphql.Schema) {
	record := func(kind, name string, f *graphql.FieldDefinition) {
		r.RegisterEndpoint(registry.Endpoint{
			Service:     service,
			Name:        name,
			Transport:   registry.GraphQL,
			Method:      kind,
			Path:        path,
			Description: f.Description,
			Args:        extractArgs(f.Args),
			ReturnType:  typeString(f.Type),
		})
	}
	if q := schema.QueryType(); q != nil {
		for name, f := range q.Fields() {
			record("query", name, f)
		}
	}
	if m := schema.MutationType(); m != nil {
		for name, f := range m.Fields() {
			record("mutation", name, f)
		}
	}
	if s := schema.SubscriptionType(); s != nil {
		for name, f := range s.Fields() {
			record("subscription", name, f)
		}
	}
}

func extractArgs(args []*graphql.Argument) []registry.GraphQLArg {
	if len(args) == 0 {
		return nil
	}
	out := make([]registry.GraphQLArg, 0, len(args))
	for _, a := range args {
		out = append(out, registry.GraphQLArg{
			Name:        a.Name(),
			Type:        typeString(a.Type),
			Description: a.Description(),
		})
	}
	return out
}

// typeString renders a graphql-go type as an SDL-like string ("String!", "[Int]").
// graphql-go's types implement fmt.Stringer for this purpose.
func typeString(t graphql.Type) string {
	if t == nil {
		return ""
	}
	return t.String()
}
