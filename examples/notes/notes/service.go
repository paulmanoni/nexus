package notes

import "github.com/paulmanoni/nexus"

// Service groups every notes endpoint under one node in the dashboard's
// architecture graph. Handlers take *Service as their first param to ground
// under it.
type Service struct{ *nexus.Service }

// @provide
func NewService(app *nexus.App) *Service {
	return &Service{app.Service("notes").Describe("Notes API")}
}
