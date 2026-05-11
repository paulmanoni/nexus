package manifest

import "encoding/json"

// This file extends the manifest schema with the "inputs" surface that
// makes nexus.deploy.yaml a complete contract between a binary and its
// orchestration platform (Laravel-Cloud style). Everything here is
// additive — existing manifests with no Environments/Secrets/Files/
// Hooks/Overrides blocks parse identically to before.
//
// Resolution model:
//   base inputs declared by the binary
//     → overrides for the active Environment (deep-merged via merge.go)
//       → platform-injected env vars (operator-set values)
//         → boot-time validation against EnvValidation rules
//
// The merge happens in manifest.MergeOverrides (pure function).
// Validation happens in two stages: Lint() rejects the manifest at
// write time for structural problems; Validate() rejects effective
// values at boot for content problems.

// Environment is one named target the binary can be deployed to —
// "production", "staging", "preview", etc. Overrides reference it by
// name; the orchestration platform routes deploys at the environment
// level.
type Environment struct {
	Name      string     `json:"name" yaml:"name"`
	Domain    string     `json:"domain,omitempty" yaml:"domain,omitempty"`
	Autoscale *Autoscale `json:"autoscale,omitempty" yaml:"autoscale,omitempty"`
	// TTL marks the environment as ephemeral (preview/PR deploys).
	// Format: Go duration string ("7d" requires the platform's parser;
	// time.ParseDuration accepts hours/minutes/seconds only). Empty =
	// persistent environment.
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`
}

// Autoscale is the platform sizing hint per environment / deployment.
// Min/Max are replica counts; the platform decides metrics + policy.
type Autoscale struct {
	Min int `json:"min,omitempty" yaml:"min,omitempty"`
	Max int `json:"max,omitempty" yaml:"max,omitempty"`
}

// Secret is a sensitive input the operator supplies through the
// platform's encrypted store. Distinct from EnvVar so the platform UI
// can render secrets with redacted values + rotation reminders, and so
// the manifest hash doesn't entangle with secret values.
type Secret struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	// EnvScoped means each environment gets its own value (typical for
	// API keys that differ between prod/staging). false → one value
	// shared across every environment (rare; usually a misconfiguration).
	EnvScoped bool `json:"envScoped,omitempty" yaml:"env_scoped,omitempty"`
	// RotateAlert is a platform reminder: emit a warning once the
	// secret hasn't been written for this long. Go-duration format on
	// the framework side; platform parses ("90d" etc.) per its own
	// rules. Optional.
	RotateAlert string         `json:"rotateAlert,omitempty" yaml:"rotate_alert,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// File is a mounted blob — TLS bundles, JSON config overrides, etc.
// Larger than an env var would carry cleanly. The platform writes the
// bytes to Path at deploy time; Mode controls filesystem permissions.
type File struct {
	Name        string `json:"name" yaml:"name"`
	Path        string `json:"path" yaml:"path"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Mode is the octal file mode (e.g. 0400 = read-only owner). 0
	// means the platform picks (typically 0644).
	Mode int `json:"mode,omitempty" yaml:"mode,omitempty"`
	// Secret=true → encrypt at rest, redact in any UI rendering of
	// the file's contents.
	Secret    bool `json:"secret,omitempty" yaml:"secret,omitempty"`
	EnvScoped bool `json:"envScoped,omitempty" yaml:"env_scoped,omitempty"`
	// JSONSchema is the optional shape the file's contents must
	// satisfy (Draft 2020-12). The platform validates pre-deploy and
	// rejects an override that violates the schema.
	JSONSchema json.RawMessage `json:"schema,omitempty" yaml:"schema,omitempty"`
}

// EnvValidation constrains the effective value of an env var or
// secret. Applied at boot (or pre-render, when the platform is doing
// the merge). All fields are optional and AND together.
type EnvValidation struct {
	Enum   []string `json:"enum,omitempty" yaml:"enum,omitempty"`
	Regex  string   `json:"regex,omitempty" yaml:"regex,omitempty"`
	Min    *int     `json:"min,omitempty" yaml:"min,omitempty"`       // numeric values
	Max    *int     `json:"max,omitempty" yaml:"max,omitempty"`       // numeric values
	Length *Range   `json:"length,omitempty" yaml:"length,omitempty"` // string length
}

// Range is [Min, Max] inclusive. Nil pointers mean unbounded on that
// end; using pointer-int lets the zero value be a meaningful bound.
type Range struct {
	Min *int `json:"min,omitempty" yaml:"min,omitempty"`
	Max *int `json:"max,omitempty" yaml:"max,omitempty"`
}

// Hooks are platform-orchestrated commands run outside the binary at
// well-defined deploy phases. Each list runs sequentially; non-zero
// exit aborts the deploy. Env vars resolved through the same merge
// pipeline are available to every hook command.
type Hooks struct {
	// Build runs on the platform's build node before the image is
	// finalized. Typical use: `go build`, `npm run build`, vendoring.
	Build []string `json:"build,omitempty" yaml:"build,omitempty"`
	// Predeploy runs after the new image is built and before traffic
	// shifts. Typical use: schema migrations, cache warmups.
	Predeploy []string `json:"predeploy,omitempty" yaml:"predeploy,omitempty"`
	// Postdeploy runs after the new release is live. Typical use: CDN
	// purges, Slack notifications, smoke tests against the new URL.
	Postdeploy []string `json:"postdeploy,omitempty" yaml:"postdeploy,omitempty"`
}

// Override is the per-environment diff applied on top of the base
// inputs at merge time. Keys absent here inherit from base; keys with
// scalar values lock the field (operator UI marks it read-only); keys
// with object values deep-merge into the base spec; explicit null
// removes the field from the effective manifest for this environment.
//
// Overrides cannot introduce inputs that aren't declared in the base —
// the lint step rejects that. The base manifest is the authoritative
// answer to "what does this binary need?".
type Override struct {
	// Scalar locks live in Env / SecretLocks / FileLocks. Object diffs
	// (validation changes, required flips, expose_as additions) live
	// in EnvSpecs / SecretSpecs / FileSpecs / Services. Null removals
	// live in Removed. Splitting by shape sidesteps JSON's any-typed
	// nightmare for merge code.
	Env         map[string]string       `json:"env,omitempty" yaml:"env,omitempty"`
	EnvSpecs    map[string]EnvVarPatch  `json:"envSpecs,omitempty" yaml:"env_specs,omitempty"`
	SecretSpecs map[string]SecretPatch  `json:"secretSpecs,omitempty" yaml:"secret_specs,omitempty"`
	FileSpecs   map[string]FilePatch    `json:"fileSpecs,omitempty" yaml:"file_specs,omitempty"`
	Services    map[string]ServicePatch `json:"services,omitempty" yaml:"services,omitempty"`
	// Removed holds dotted paths (e.g. "secrets.STRIPE_API_KEY",
	// "files.tls_bundle") that don't exist in this environment.
	Removed []string `json:"removed,omitempty" yaml:"removed,omitempty"`
	// Hooks fully replaces the base Hooks block for this environment.
	// nil means inherit; non-nil replaces.
	Hooks *Hooks `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// EnvVarPatch is the subset of EnvVar fields an override is allowed to
// adjust. Pointer fields let "absent" mean "inherit", while non-nil
// pointer means "set". Default and EnvScoped and Validation can all be
// adjusted; Name and Source cannot.
type EnvVarPatch struct {
	Description *string        `json:"description,omitempty" yaml:"description,omitempty"`
	Required    *bool          `json:"required,omitempty" yaml:"required,omitempty"`
	Default     *string        `json:"default,omitempty" yaml:"default,omitempty"`
	EnvScoped   *bool          `json:"envScoped,omitempty" yaml:"env_scoped,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// SecretPatch is the override-adjustable subset of Secret.
type SecretPatch struct {
	Description *string        `json:"description,omitempty" yaml:"description,omitempty"`
	Required    *bool          `json:"required,omitempty" yaml:"required,omitempty"`
	EnvScoped   *bool          `json:"envScoped,omitempty" yaml:"env_scoped,omitempty"`
	RotateAlert *string        `json:"rotateAlert,omitempty" yaml:"rotate_alert,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" yaml:"validation,omitempty"`
}

// FilePatch is the override-adjustable subset of File.
type FilePatch struct {
	Description *string `json:"description,omitempty" yaml:"description,omitempty"`
	Mode        *int    `json:"mode,omitempty" yaml:"mode,omitempty"`
	Secret      *bool   `json:"secret,omitempty" yaml:"secret,omitempty"`
	EnvScoped   *bool   `json:"envScoped,omitempty" yaml:"env_scoped,omitempty"`
}

// ServicePatch is the override-adjustable subset of ServiceNeed. Most
// useful for sizing/backup tweaks per environment (large in prod,
// small in preview) and for adding new ExposeAs entries.
type ServicePatch struct {
	Version  *string           `json:"version,omitempty" yaml:"version,omitempty"`
	Size     *string           `json:"size,omitempty" yaml:"size,omitempty"`
	Backup   *string           `json:"backup,omitempty" yaml:"backup,omitempty"`
	ExposeAs map[string]string `json:"exposeAs,omitempty" yaml:"expose_as,omitempty"`
	// Ephemeral=true tells the platform to tear down the provisioned
	// resource when the environment is destroyed (typical for preview
	// envs where you don't want orphaned databases lying around).
	Ephemeral *bool `json:"ephemeral,omitempty" yaml:"ephemeral,omitempty"`
	// Optional flips a service to non-required for this environment
	// (e.g. payment service in preview).
	Optional *bool `json:"optional,omitempty" yaml:"optional,omitempty"`
}