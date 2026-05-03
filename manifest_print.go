package nexus

import (
	"fmt"
	"io"
	"os"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/manifest"
)

// printManifestEnv is consulted by Run to decide whether to short-
// circuit into print mode. Mirrors manifest.EnvVarPrintAndExit so
// options.go doesn't need an import that crosses out and back into
// the manifest package.
const printManifestEnv = manifest.EnvVarPrintAndExit

// printManifestAndExitIfRequested is the build-time/upload-time
// extraction path that replaces the rejected `/__nexus/manifest`
// HTTP endpoint.
//
// The orchestration platform invokes the built image once, with
// NEXUS_PRINT_MANIFEST=1 set:
//
//	docker run --rm -e NEXUS_PRINT_MANIFEST=1 <image>
//
// The container boots its fx graph far enough to resolve every
// module-level declaration (DeclareEnv / DeclareService / UseVolume /
// AddStartupTask), prints the assembled manifest as JSON to stdout,
// and exits 0 — without binding listeners, without dialing Redis or
// Postgres, without firing any startup task. The orchestration
// platform stores the captured JSON on the build row and uses it to
// plan the actual deploy (sidecars, env, volumes, migrations).
//
// Why a build-time exit and not an HTTP endpoint:
//   - Manifest is needed BEFORE the app runs to plan dependencies; a
//     runtime endpoint is too late and would create a chicken-and-egg
//     for sidecars/env that would otherwise be needed to boot.
//   - Build-time JSON is diffable in CI to catch breaking deploy-
//     config changes before merge.
//   - No public network surface to defend.
//
// Wiring (intended; this function is the seam, called from Run):
//
//	func Run(cfg Config, opts ...Option) {
//	    cfg = resolveConfig(cfg)
//	    if err := validateTopology(cfg); err != nil { panic(err) }
//
//	    if os.Getenv(manifest.EnvVarPrintAndExit) == "1" {
//	        printManifestAndExitIfRequested(cfg, opts) // exits 0 or non-0
//	    }
//
//	    // ...existing fx.New(...).Run() path unchanged...
//	}
//
// This function builds the fx graph, populates *App, prints, and
// exits — it never returns to the caller. Side-effect contract: any
// EnvProvider / ServiceDependencyProvider / VolumeProvider
// implementation MUST be cheap and side-effect-free, because their
// methods are called as part of manifest assembly. Constructors that
// dial external systems should not register declarations from inside
// themselves; declare at module level instead (see "Integration"
// step 4 in manifest/manifest.go).
func printManifestAndExitIfRequested(cfg Config, opts []Option) {
	// Print mode must produce JSON-and-only-JSON on stdout — anything
	// else breaks downstream parsers (`nexus reconcile`, `nexus build
	// --emit-manifest`, the orchestrator's extractManifest). Silence
	// Gin's route-registration debug noise (which fires during fx
	// graph construction below as modules wire endpoints) by switching
	// it to release mode AND redirecting its writers to discard. We
	// hold both belts: ReleaseMode suppresses most lines, the writer
	// swap catches anything that leaks regardless of mode.
	gin.SetMode(gin.ReleaseMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	// Build the same option chain Run uses, MINUS fxLateOptions —
	// fxLateOptions invokes autoMountGraphQL which registers HTTP
	// routes; we don't need that for the manifest and skipping it
	// keeps print mode strictly cheaper than a real boot.
	all := append([]fx.Option{fxEarlyOptions(cfg), autoClientOptions()}, unwrap(opts)...)

	// Drop the lifecycle invoke from fxEarlyOptions if it ever grows
	// side-effecting OnStart hooks beyond listener bind (today
	// registerLifecycle's OnStart binds listeners — we'd rather not
	// run that loop at all, so we'd ideally swap fxEarlyOptions for
	// a print-only variant. Tracked as an open item; the simpler
	// route below uses fx.Populate before .Start() is called, which
	// keeps OnStart hooks unfired.

	all = append(all, fx.NopLogger)

	var app *App
	all = append(all, fx.Populate(&app))

	fxApp := fx.New(all...)
	if err := fxApp.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: graph error:", err)
		os.Exit(2)
	}
	if app == nil {
		// fx.New ran but Populate didn't fill *App — should not
		// happen given fxEarlyOptions provides New, but guard anyway
		// so a future refactor doesn't silently print {}.
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: *App not provided by graph")
		os.Exit(2)
	}

	// Build manifest from inputs. manifestInputs is the *App method
	// added in step 1 of the integration sequence — it gathers
	// providers, registry snapshots, and listener ports into
	// manifest.Inputs. Until that lands, this call is a stub returning
	// a near-empty manifest with just identity + ports populated from
	// what *App already exposes.
	in := app.manifestInputs()
	if err := manifest.PrintJSON(os.Stdout, manifest.Build(in)); err != nil {
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: encode:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// (manifestInputs lives on *App in manifest_app.go alongside the
// declaration store and option helpers.)