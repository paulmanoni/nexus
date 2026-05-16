package nexus

import (
	"errors"
	"io/fs"
	"log"
	"os"

	"go.uber.org/fx"
)

// DefaultManifestPath is the file the framework auto-loads from the
// current working directory at boot. Operators who need a different
// path call `app.LoadDeployManifest("alt-path.yaml")` from their own
// nexus.Invoke — that runs AFTER auto-load and the framework's
// declarations are idempotent (last writer wins on each block) so
// explicit overrides land cleanly.
const DefaultManifestPath = "nexus.deploy.yaml"

// autoManifestEnvSkip lets operators opt out of auto-loading when
// they need to run multiple manifest files via explicit
// LoadDeployManifest calls without the auto-loader stepping on the
// first one. Set NEXUS_SKIP_MANIFEST_AUTOLOAD=1 to disable.
const autoManifestEnvSkip = "NEXUS_SKIP_MANIFEST_AUTOLOAD"

// autoManifestOptions returns the fx.Invoke that auto-loads
// nexus.deploy.yaml from the current working directory if it
// exists. Lets plugins (extension/tls, /cors, /errors, ...) read
// their per-environment config via app.EffectiveManifest() without
// the operator having to write the boilerplate:
//
//	nexus.Invoke(func(app *nexus.App) {
//	    if err := app.LoadDeployManifest("nexus.deploy.yaml"); err != nil {
//	        panic(err)
//	    }
//	})
//
// Behavior:
//
//   - File missing  → silent skip. Apps without a manifest boot
//                     normally; the operator hasn't adopted the
//                     cloud-shaped input surface yet.
//   - File present  → parse + Declare each block. Existing Declare*
//                     calls from user code keep their semantics
//                     (last writer wins per block).
//   - Parse error   → panic with a clear message. A malformed
//                     manifest is an operator bug; failing fast at
//                     boot is the right behavior. Same shape as
//                     LoadDeployManifest's documented contract.
//   - Other I/O err → logged + skipped. Permission glitches in a
//                     dev environment shouldn't crash the app.
//
// Disable with NEXUS_SKIP_MANIFEST_AUTOLOAD=1 for the rare case
// where two manifest files must be loaded in a specific order.
func autoManifestOptions() fx.Option {
	if os.Getenv(autoManifestEnvSkip) == "1" {
		return fx.Options()
	}
	return fx.Invoke(func(app *App) {
		path := DefaultManifestPath
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				// Permission denied, I/O error — log it but keep
				// booting; the operator can recover by fixing the
				// file ACL and restarting.
				log.Printf("nexus: manifest auto-load: stat %s: %v", path, err)
			}
			return
		}
		if err := app.LoadDeployManifest(path); err != nil {
			// Parse error / schema mismatch / etc. Operator bug —
			// surface loud rather than silently dropping the
			// manifest and watching plugins fail downstream with
			// confusing "Config.X is required" messages.
			panic("nexus: manifest auto-load: " + err.Error())
		}
	})
}
