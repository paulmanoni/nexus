package nexus

import "sync"

// Module-deployment registry: maps module name → DeployAs tag for
// modules that don't carry an explicit nexus.DeployAs(...) option in
// source. Populated by a codegen'd init() in zz_deploy_gen.go from
// the manifest's `deployments[X].owns: [name]` mapping — so a module
// listed under a non-monolith deployment's owns block gets its tag
// inferred without the user typing nexus.DeployAs("foo-svc").
//
// Explicit DeployAs in source still wins when both are present.
//
// User code does not call RegisterModuleDeployment directly; the
// build tool emits the registrations.

var (
	moduleDeployMu      sync.RWMutex
	moduleDeployByName  = map[string]string{}
)

// RegisterModuleDeployment associates a module name with a DeployAs
// tag. Called from a codegen'd init() block when the manifest
// declares this module under a split-unit deployment's owns list.
//
// Re-registration replaces the previous mapping (last write wins),
// matching how SetDeploymentDefaults handles repeated calls.
func RegisterModuleDeployment(moduleName, tag string) {
	moduleDeployMu.Lock()
	defer moduleDeployMu.Unlock()
	moduleDeployByName[moduleName] = tag
}

// inferredDeployTag returns the manifest-derived tag for moduleName,
// or "" when no registration exists. Read by Module() at construction
// time as the fallback when no explicit deployTagOption is present.
func inferredDeployTag(moduleName string) string {
	moduleDeployMu.RLock()
	defer moduleDeployMu.RUnlock()
	return moduleDeployByName[moduleName]
}
