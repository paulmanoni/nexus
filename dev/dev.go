// Package dev is what an app sees of `nexus dev`: whether it runs under it,
// and the state it can carry across the rebuilds the dev loop makes.
package dev

import "os"

// Env is the variable `nexus dev` sets to "1" on the app it runs.
// Production binaries never see it: `nexus build` doesn't set it and
// operator-launched units don't either.
const Env = "NEXUS_DEV"

// Enabled reports whether the app runs under `nexus dev`.
func Enabled() bool { return os.Getenv(Env) == "1" }

// RootEnv overrides the directory a dev-mode app reads its frontend bundle
// and other embedded trees from (default: the working directory, which is
// how //go:embed paths are declared). nexus dev sets it to the project dir,
// so an app started from elsewhere still resolves them.
const RootEnv = "NEXUS_DEV_ROOT"
