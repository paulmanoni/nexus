package users

import "github.com/paulmanoni/nexus"

// Module is the wired declaration. The DeployAs tag is inferred from
// nexus.deploy.yaml — the manifest's `users-svc.owns: [users]` entry
// tells the framework this module belongs to the users-svc deployment
// unit. Add an explicit nexus.DeployAs("users-svc") here if you want
// the tag pinned in source (it overrides the manifest inference).
var Module = nexus.Module("users",
	nexus.Provide(NewService),
	nexus.AsRest("GET", "/users/:id", NewGet),
	nexus.AsRest("GET", "/users", NewList),
	nexus.AsQuery(NewSearch),
)
