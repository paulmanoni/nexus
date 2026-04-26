package users

import "github.com/paulmanoni/nexus"

// Module is the wired declaration. DeployAs("users-svc") names this
// module's deployment unit — `nexus build --deployment X` uses it to
// decide whether to keep this package's hand-written code or
// substitute the shadow stub.
var Module = nexus.Module("users",
	nexus.DeployAs("users-svc"),
	nexus.Provide(NewService),
	nexus.AsRest("GET", "/users/:id", NewGet),
	nexus.AsRest("GET", "/users", NewList),
	nexus.AsQuery(NewSearch),
)
