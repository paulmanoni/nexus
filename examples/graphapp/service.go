package main

import "github.com/paulmanoni/nexus"

// AdvertsService is a typed wrapper around *nexus.Service. Fx routes the
// service by type, so distinct graphs (*AdvertsService vs *PetsService)
// never clash in a single app.
type AdvertsService struct {
	*nexus.Service
}

// NewAdvertsService declares the adverts service on the dashboard. No
// AtGraphQL (default "/graphql") and no Using(...) — fxmod's auto-mount
// attaches resources declared by dep managers (via NexusResourceProvider)
// and mounts the schema automatically.
func NewAdvertsService(app *nexus.App) *AdvertsService {
	return &AdvertsService{Service: app.Service("adverts").Describe("Job adverts + question bank")}
}
