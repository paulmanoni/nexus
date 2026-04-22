package main

import (
	"go.uber.org/fx"

	"nexus/graphfx"
)

// advertsModule wires the adverts domain. Per-resolver middleware names, arg
// validators, and deprecation flow automatically from go-graph introspection.
var advertsModule = fx.Module("adverts",
	fx.Provide(
		graphfx.AsQuery(NewGetAllAdverts),
		graphfx.AsMutation(NewCreateAdvert),
		graphfx.AsQuery(NewListQuestions),
	),
	graphfx.ServeAt("adverts", "/graphql",
		graphfx.Describe("Job adverts + question bank"),
		graphfx.UseDefaults(),
		// No explicit Use("questions") — app.OnResourceUse(dbs) in
		// resourcesModule auto-attaches it the first time a resolver calls
		// dbs.UsingCtx(p.Context, "questions").
	),
)
