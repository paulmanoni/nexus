package manifest

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// DeployTOMLInputs is the on-disk shape of the cloud-inputs surface
// inside nexus.toml. Distinct from the framework's runtime Manifest
// type so the TOML schema can evolve independently of the JSON one
// the binary emits at NEXUS_PRINT_MANIFEST=1 print time — the two
// surfaces serve different consumers (operators vs. the platform)
// and benefit from different conventions (snake_case TOML keys,
// fewer required fields up-front).
//
// Top-level keys map to nexus.toml entries the loader recognizes
// today. Unknown keys are ignored so old manifests parse without
// errors:
//
//	[environments.production]
//	domain = "app.example.com"
//
//	[environments.staging]
//	domain = "staging.example.com"
//
//	[secrets.JWT_SIGNING_KEY]
//	required   = true
//	env_scoped = true
//
//	[files.tls_bundle]
//	path = "/etc/ssl/app/bundle.pem"
//	mode = 0o400
//
//	[hooks]
//	build     = ["go build ./..."]
//	predeploy = ["./bin/app migrate"]
//
//	[environment_overrides.production]
//	env = { LOG_LEVEL = "warn" }
//	[environment_overrides.production.services.main_db]
//	size   = "large"
//	backup = "hourly"
//
// Maps are keyed by logical name (env name, secret name, etc.) for
// operator ergonomics. The loader converts to the framework's
// slice-of-named-structs shape at Load time so the runtime path
// stays unchanged.
type DeployTOMLInputs struct {
	Environments         map[string]EnvironmentTOML `toml:"environments,omitempty"`
	Secrets              map[string]Secret          `toml:"secrets,omitempty"`
	Files                map[string]File            `toml:"files,omitempty"`
	Hooks                *Hooks                     `toml:"hooks,omitempty"`
	TLS                  *TLSBlock                  `toml:"tls,omitempty"`
	CORS                 *CORSBlock                 `toml:"cors,omitempty"`
	Errors               *ErrorsBlock               `toml:"errors,omitempty"`
	EnvironmentOverrides map[string]Override        `toml:"environment_overrides,omitempty"`

	// Pre-existing top-level blocks (auto-populated by `nexus
	// reconcile` from binary print mode, or hand-written by
	// operators). Not part of the v0.42 "cloud inputs" surface but
	// surfaced through the same loader so tools that consume TOML
	// only (doctor, lint) see the complete declared shape.
	Env      map[string]EnvVar      `toml:"env,omitempty"`
	Services map[string]ServiceNeed `toml:"services,omitempty"`
}

// EnvironmentTOML mirrors Environment but doesn't carry a Name field
// (the table key in [environments.X] is the name). Lifting Name out
// of the value keeps the operator-facing TOML clean:
//
//	[environments.production]
//	domain = "app.example.com"
//
// rather than the redundant:
//
//	[[environments]]
//	name = "production"
//	domain = "app.example.com"
type EnvironmentTOML struct {
	Domain    string     `toml:"domain,omitempty"`
	Autoscale *Autoscale `toml:"autoscale,omitempty"`
	TTL       string     `toml:"ttl,omitempty"`
}

// LoadInputsTOML parses a TOML document into a partial Manifest with
// only the inputs surface populated (Environments, Secrets, Files,
// Hooks, Overrides). The other Manifest fields stay zero-valued —
// callers merge this result with the framework-built base manifest.
//
// Behavior:
//
//   - Top-level keys outside the inputs surface (deployments, peers,
//     services from reconcile, etc.) are silently ignored. The same
//     file can declare both surfaces without conflict.
//   - Table-keyed inputs are converted to named-struct slices: the
//     TOML table key fills the Name field of each entry.
//   - Empty / missing sections produce nil slices and nil pointers
//     (no allocations), so the result round-trips identically through
//     MergeOverrides as if those sections were never declared.
//
// Returns an error only on TOML syntax problems. Schema validation
// (duplicate names, malformed validation rules, unknown override
// keys) lives in Lint — callers should run Lint after Load.
func LoadInputsTOML(data []byte) (Manifest, error) {
	var raw DeployTOMLInputs
	if err := toml.Unmarshal(data, &raw); err != nil {
		return Manifest{}, fmt.Errorf("manifest: parse TOML inputs: %w", err)
	}
	return materializeInputs(raw), nil
}

// LoadInputsTOMLFile is the file-path convenience over LoadInputsTOML.
// Reads the file then delegates. Errors include the path so a missing
// file or TOML syntax problem is easy to locate.
func LoadInputsTOMLFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: read %s: %w", path, err)
	}
	m, err := LoadInputsTOML(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: %s: %w", path, err)
	}
	return m, nil
}

// materializeInputs converts the TOML DTO into the framework's
// runtime Manifest shape. Pure function — no I/O, no globals.
func materializeInputs(raw DeployTOMLInputs) Manifest {
	var m Manifest

	if len(raw.Environments) > 0 {
		m.Environments = make([]Environment, 0, len(raw.Environments))
		for name, e := range raw.Environments {
			m.Environments = append(m.Environments, Environment{
				Name:      name,
				Domain:    e.Domain,
				Autoscale: e.Autoscale,
				TTL:       e.TTL,
			})
		}
		// Stable order for deterministic Lint output + manifest hashing.
		sortInputsByName(m.Environments, func(e Environment) string { return e.Name })
	}

	if len(raw.Secrets) > 0 {
		m.Secrets = make([]Secret, 0, len(raw.Secrets))
		for name, s := range raw.Secrets {
			s.Name = name // table key wins over any inline name
			m.Secrets = append(m.Secrets, s)
		}
		sortInputsByName(m.Secrets, func(s Secret) string { return s.Name })
	}

	if len(raw.Files) > 0 {
		m.Files = make([]File, 0, len(raw.Files))
		for name, f := range raw.Files {
			f.Name = name
			m.Files = append(m.Files, f)
		}
		sortInputsByName(m.Files, func(f File) string { return f.Name })
	}

	if raw.Hooks != nil {
		h := *raw.Hooks
		m.Hooks = &h
	}

	if raw.TLS != nil {
		t := *raw.TLS
		// Defensive copy of the Domains slice so callers can't mutate
		// the loader's input through the materialized manifest.
		if t.Domains != nil {
			t.Domains = append([]string(nil), t.Domains...)
		}
		m.TLS = &t
	}

	if raw.CORS != nil {
		c := *raw.CORS
		// Defensive copies of every slice field so a mutation on
		// the loader's input can't reach the materialized manifest.
		if c.AllowOrigins != nil {
			c.AllowOrigins = append([]string(nil), c.AllowOrigins...)
		}
		if c.AllowMethods != nil {
			c.AllowMethods = append([]string(nil), c.AllowMethods...)
		}
		if c.AllowHeaders != nil {
			c.AllowHeaders = append([]string(nil), c.AllowHeaders...)
		}
		if c.ExposeHeaders != nil {
			c.ExposeHeaders = append([]string(nil), c.ExposeHeaders...)
		}
		m.CORS = &c
	}

	if raw.Errors != nil {
		e := *raw.Errors
		if e.IgnorePaths != nil {
			e.IgnorePaths = append([]string(nil), e.IgnorePaths...)
		}
		if e.SampleRate != nil {
			v := *e.SampleRate
			e.SampleRate = &v
		}
		m.Errors = &e
	}

	if len(raw.Env) > 0 {
		m.Env = make([]EnvVar, 0, len(raw.Env))
		for name, e := range raw.Env {
			e.Name = name
			m.Env = append(m.Env, e)
		}
		sortInputsByName(m.Env, func(e EnvVar) string { return e.Name })
	}

	if len(raw.Services) > 0 {
		m.Services = make([]ServiceNeed, 0, len(raw.Services))
		for name, s := range raw.Services {
			s.Name = name
			m.Services = append(m.Services, s)
		}
		sortInputsByName(m.Services, func(s ServiceNeed) string { return s.Name })
	}

	if len(raw.EnvironmentOverrides) > 0 {
		m.Overrides = make(map[string]Override, len(raw.EnvironmentOverrides))
		for env, ov := range raw.EnvironmentOverrides {
			m.Overrides[env] = ov
		}
	}

	return m
}

// sortInputsByName is a tiny generic sort helper. Keeps the
// materialize* functions free of inline sort.Slice noise.
func sortInputsByName[T any](items []T, name func(T) string) {
	sortBy(items, func(a, b T) bool { return name(a) < name(b) })
}

func sortBy[T any](items []T, less func(a, b T) bool) {
	// In-place insertion sort — N is typically <20 here (number of
	// environments / secrets per app), so the algorithm choice is
	// irrelevant; this avoids an import of sort.Slice's reflection.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}