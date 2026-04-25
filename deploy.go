// Package nexus deployment annotations.
//
// DeployAs marks a module as a candidate deployment unit — when the framework
// later supports independent rollout (`nexus gen clients` + `NEXUS_DEPLOYMENT`
// boot selector), the tag identifies which binary the module belongs to. In
// today's monolith mode the tag is metadata only: it surfaces on the
// dashboard so readers can see the planned split topology before any
// splitting happens.
//
//	var users = nexus.Module("users",
//	    nexus.DeployAs("users-svc"),
//	    nexus.Provide(NewUsersService),
//	    nexus.AsRest("GET", "/users/:id", NewGetUser),
//	)
//
// Untagged modules are "always local" — they ride along with whichever
// deployment is active. Reserve DeployAs for modules with a real chance of
// being peeled out (separate codebase boundary, separate scaling needs,
// separate on-call rotation).
package nexus

import "go.uber.org/fx"

// DeployAs records the deployment tag for the enclosing nexus.Module.
// Multiple DeployAs calls in one Module are not allowed — the last one
// wins, matching the way Module() handles duplicated RoutePrefix today.
func DeployAs(tag string) Option { return deployTagOption{tag: tag} }

// deployTagOption is the Option carrying the tag. It contributes nothing
// to the fx graph; Module() picks it out of its opts list and stamps the
// tag onto every annotator-supporting child.
type deployTagOption struct{ tag string }

func (deployTagOption) nexusOption() fx.Option { return fx.Options() }

// deploymentAnnotator is implemented by AsRest / AsQuery / AsMutation /
// AsWS option types so Module(..., DeployAs("x"), ...) can stamp the tag
// onto every endpoint registration. Module's existing moduleAnnotator
// pattern is the precedent.
type deploymentAnnotator interface {
	setDeployment(tag string)
}