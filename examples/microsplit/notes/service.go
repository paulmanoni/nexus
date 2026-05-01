package notes

import "github.com/paulmanoni/nexus"

// Service exists purely so the framework's GraphQL auto-mount has a
// service to attach the generated ops onto. AsCRUD doesn't need any
// behaviour from it — the per-request resolver does the real work.
//
// In a real module you'd add cross-module methods on Service for
// other modules to call (the shadow generator picks those up); the
// CRUD surface itself stays generated.
type Service struct{ *nexus.Service }

// NewService is fx-resolved at boot. The Describe text shows up on
// the dashboard's module card, so this is the right place to give
// human reviewers a one-line summary of the module.
func NewService(app *nexus.App) *Service {
	return &Service{
		Service: app.Service("Notes service").Describe("CRUD-only notes — no hand-written handlers."),
	}
}