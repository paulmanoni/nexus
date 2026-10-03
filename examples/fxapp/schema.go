package main

import (
	"context"

	"github.com/paulmanoni/nexus/v2"
)

// GraphService serves the GraphQL demo; its handlers are plain methods and
// the schema is derived from their argument and result types.
type GraphService struct{ *nexus.Service }

func NewGraphService(app *nexus.App) *GraphService {
	return &GraphService{app.Service("graph").Describe("GraphQL demo")}
}

type Pet struct {
	Name    string `json:"name"`
	Species string `json:"species"`
}

type Health struct {
	OK bool `json:"ok"`
}

type PetArgs struct {
	Name string `graphql:"name,required"`
}

type RenamePetArgs struct {
	OldName string `graphql:"oldName,required"`
	NewName string `graphql:"newName,required"`
}

func (s *GraphService) Pet(ctx context.Context, in PetArgs) (*Pet, error) {
	return &Pet{Name: in.Name, Species: "dog"}, nil
}

func (s *GraphService) Ping(ctx context.Context) (*Health, error) {
	return &Health{OK: true}, nil
}

func (s *GraphService) RenamePet(ctx context.Context, in RenamePetArgs) (*Pet, error) {
	return &Pet{Name: in.NewName, Species: "dog"}, nil
}

var graphModule = nexus.Module("graph",
	nexus.Provide(NewGraphService),
	nexus.AsQuery((*GraphService).Pet, nexus.Describe("Fetch a pet by name")),
	nexus.AsQuery((*GraphService).Ping, nexus.Describe("Health check")),
	nexus.AsMutation((*GraphService).RenamePet, nexus.Describe("Rename a pet")),
)
