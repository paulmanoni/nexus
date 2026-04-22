package main

import "github.com/graphql-go/graphql"

var petType = graphql.NewObject(graphql.ObjectConfig{
	Name: "Pet",
	Fields: graphql.Fields{
		"name":    &graphql.Field{Type: graphql.String},
		"species": &graphql.Field{Type: graphql.String},
	},
})

func buildSchema() *graphql.Schema {
	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"pet": &graphql.Field{
				Type:        petType,
				Description: "Fetch a pet by name",
				Args: graphql.FieldConfigArgument{
					"name": &graphql.ArgumentConfig{
						Type:        graphql.NewNonNull(graphql.String),
						Description: "Name of the pet",
					},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return map[string]any{"name": p.Args["name"], "species": "dog"}, nil
				},
			},
			"ping": &graphql.Field{
				Type:        graphql.String,
				Description: "Health check",
				Resolve:     func(graphql.ResolveParams) (any, error) { return "pong", nil },
			},
		},
	})
	mutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"renamePet": &graphql.Field{
				Type:        petType,
				Description: "Rename a pet",
				Args: graphql.FieldConfigArgument{
					"oldName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"newName": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return map[string]any{"name": p.Args["newName"], "species": "dog"}, nil
				},
			},
		},
	})
	s, err := graphql.NewSchema(graphql.SchemaConfig{Query: query, Mutation: mutation})
	if err != nil {
		panic(err)
	}
	return &s
}
