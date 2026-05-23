package manifest

import "encoding/json"

// This file extends the manifest schema with the "inputs" surface that
// makes nexus.toml a complete contract between a binary and its
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
	Name      string     `json:"name" toml:"name"`
	Domain    string     `json:"domain,omitempty" toml:"domain,omitempty"`
	Autoscale *Autoscale `json:"autoscale,omitempty" toml:"autoscale,omitempty"`
	// TTL marks the environment as ephemeral (preview/PR deploys).
	// Format: Go duration string ("7d" requires the platform's parser;
	// time.ParseDuration accepts hours/minutes/seconds only). Empty =
	// persistent environment.
	TTL string `json:"ttl,omitempty" toml:"ttl,omitempty"`
}

// Autoscale is the platform sizing hint per environment / deployment.
// Min/Max are replica counts; the platform decides metrics + policy.
type Autoscale struct {
	Min int `json:"min,omitempty" toml:"min,omitempty"`
	Max int `json:"max,omitempty" toml:"max,omitempty"`
}

// Secret is a sensitive input the operator supplies through the
// platform's encrypted store. Distinct from EnvVar so the platform UI
// can render secrets with redacted values + rotation reminders, and so
// the manifest hash doesn't entangle with secret values.
type Secret struct {
	Name        string `json:"name" toml:"name"`
	Description string `json:"description,omitempty" toml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" toml:"required,omitempty"`
	// EnvScoped means each environment gets its own value (typical for
	// API keys that differ between prod/staging). false → one value
	// shared across every environment (rare; usually a misconfiguration).
	EnvScoped bool `json:"envScoped,omitempty" toml:"env_scoped,omitempty"`
	// RotateAlert is a platform reminder: emit a warning once the
	// secret hasn't been written for this long. Go-duration format on
	// the framework side; platform parses ("90d" etc.) per its own
	// rules. Optional.
	RotateAlert string         `json:"rotateAlert,omitempty" toml:"rotate_alert,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" toml:"validation,omitempty"`
}

// File is a mounted blob — TLS bundles, JSON config overrides, etc.
// Larger than an env var would carry cleanly. The platform writes the
// bytes to Path at deploy time; Mode controls filesystem permissions.
type File struct {
	Name        string `json:"name" toml:"name"`
	Path        string `json:"path" toml:"path"`
	Description string `json:"description,omitempty" toml:"description,omitempty"`
	// Mode is the octal file mode (e.g. 0400 = read-only owner). 0
	// means the platform picks (typically 0644).
	Mode int `json:"mode,omitempty" toml:"mode,omitempty"`
	// Secret=true → encrypt at rest, redact in any UI rendering of
	// the file's contents.
	Secret    bool `json:"secret,omitempty" toml:"secret,omitempty"`
	EnvScoped bool `json:"envScoped,omitempty" toml:"env_scoped,omitempty"`
	// JSONSchema is the optional shape the file's contents must
	// satisfy (Draft 2020-12). The platform validates pre-deploy and
	// rejects an override that violates the schema.
	JSONSchema json.RawMessage `json:"schema,omitempty" toml:"schema,omitempty"`
}

// EnvValidation constrains the effective value of an env var or
// secret. Applied at boot (or pre-render, when the platform is doing
// the merge). All fields are optional and AND together.
type EnvValidation struct {
	Enum   []string `json:"enum,omitempty" toml:"enum,omitempty"`
	Regex  string   `json:"regex,omitempty" toml:"regex,omitempty"`
	Min    *int     `json:"min,omitempty" toml:"min,omitempty"`       // numeric values
	Max    *int     `json:"max,omitempty" toml:"max,omitempty"`       // numeric values
	Length *Range   `json:"length,omitempty" toml:"length,omitempty"` // string length
}

// Range is [Min, Max] inclusive. Nil pointers mean unbounded on that
// end; using pointer-int lets the zero value be a meaningful bound.
type Range struct {
	Min *int `json:"min,omitempty" toml:"min,omitempty"`
	Max *int `json:"max,omitempty" toml:"max,omitempty"`
}

// Hooks are platform-orchestrated commands run outside the binary at
// well-defined deploy phases. Each list runs sequentially; non-zero
// exit aborts the deploy. Env vars resolved through the same merge
// pipeline are available to every hook command.
type Hooks struct {
	// Build runs on the platform's build node before the image is
	// finalized. Typical use: `go build`, `npm run build`, vendoring.
	Build []string `json:"build,omitempty" toml:"build,omitempty"`
	// Predeploy runs after the new image is built and before traffic
	// shifts. Typical use: schema migrations, cache warmups.
	Predeploy []string `json:"predeploy,omitempty" toml:"predeploy,omitempty"`
	// Postdeploy runs after the new release is live. Typical use: CDN
	// purges, Slack notifications, smoke tests against the new URL.
	Postdeploy []string `json:"postdeploy,omitempty" toml:"postdeploy,omitempty"`
}

// TLSBlock declares the public-internet TLS configuration the binary
// wants the platform (or the in-process tls extension) to provide.
// Surfaced as a structured block instead of a soup of env vars so:
//
//   - lint/doctor can reason about it (wildcard rejection, missing
//     email, conflicting cache config)
//   - environment_overrides can replace the whole domain list cleanly
//     (production gets app.example.com; staging gets staging.app...)
//   - the dashboard's TLS tab can show "this is the declared shape"
//     alongside the runtime status
//
// Empty/nil means the binary does not request platform-managed TLS.
// The in-process tls extension may still be wired separately.
type TLSBlock struct {
	// Domains is the whitelist of hostnames the certificate manager
	// is allowed to issue for. The tls extension passes these to
	// autocert.HostWhitelist; a request for any other hostname is
	// rejected before any ACME round-trip.
	Domains []string `json:"domains,omitempty" toml:"domains,omitempty"`

	// Email is the ACME account contact. Required for production
	// Let's Encrypt; expiry warnings land here ~20 days before a
	// cert lapses if our renewal fails. Treat as oncall@.
	Email string `json:"email,omitempty" toml:"email,omitempty"`

	// CacheDir is the on-disk directory the manager uses for cert
	// storage. Operator picks the path; the orchestration platform
	// guarantees a persistent volume mounted there when relevant.
	CacheDir string `json:"cacheDir,omitempty" toml:"cache_dir,omitempty"`

	// Redirect controls whether the :80 listener 301-redirects every
	// non-ACME request to https://. Pointer so "absent" (inherit
	// extension default) is distinguishable from explicit false.
	Redirect *bool `json:"redirect,omitempty" toml:"redirect,omitempty"`

	// Staging routes ACME requests to Let's Encrypt's staging
	// directory. Use during development / preview environments to
	// avoid burning production quota.
	Staging bool `json:"staging,omitempty" toml:"staging,omitempty"`

	// Disabled, when true, tells the tls extension to no-op for this
	// environment. Useful when running behind a cloud LB that
	// already terminates TLS, or when the deploy target is a dev
	// laptop where binding :443 is not possible.
	Disabled bool `json:"disabled,omitempty" toml:"disabled,omitempty"`
}

// ErrorsBlock declares the configuration for the extension/errors
// plugin — environment / release tags, capacity, sample rate, and
// the ignore-paths list. Transports stay in code (they carry Go-only
// http.Client / function types that can't survive a YAML round-trip).
//
// Per-env overrides are the typical use: production reports at 100%
// to Sentry; preview environments sample at 10%; internal envs flip
// Disabled to avoid double-reporting through another stack.
type ErrorsBlock struct {
	// Environment is the tag every reported event carries
	// ("production", "staging", "preview"). Empty in dev.
	Environment string `json:"environment,omitempty" toml:"environment,omitempty"`

	// Release identifies the binary version. Typical wiring is
	// either a Go ldflags-injected variable or the GIT_SHA env var.
	Release string `json:"release,omitempty" toml:"release,omitempty"`

	// ServerName is the host that captured the event. Defaults to
	// os.Hostname() when empty.
	ServerName string `json:"serverName,omitempty" toml:"server_name,omitempty"`

	// Capacity is the in-memory ring-buffer size for the dashboard.
	// 0 means "use default" (100).
	Capacity int `json:"capacity,omitempty" toml:"capacity,omitempty"`

	// SampleRate is the fraction of captured events forwarded to
	// transports. Pointer-typed so the override path distinguishes
	// "absent" (inherit) from "explicit 0.0" (drop everything).
	// Domain: [0.0, 1.0]. Default: 1.0.
	SampleRate *float64 `json:"sampleRate,omitempty" toml:"sample_rate,omitempty"`

	// IgnorePaths is a list of request paths whose errors are NOT
	// captured. Default: ["/__nexus/health", "/__nexus/ready"].
	IgnorePaths []string `json:"ignorePaths,omitempty" toml:"ignore_paths,omitempty"`

	// Disabled, when true, makes the plugin a no-op for this
	// environment.
	Disabled bool `json:"disabled,omitempty" toml:"disabled,omitempty"`
}

// ErrorsPatch is the per-environment override subset of ErrorsBlock.
// Slice fields list-replace; scalar fields use pointer-set semantics
// so an override can distinguish "absent" from "explicit zero".
type ErrorsPatch struct {
	Environment *string  `json:"environment,omitempty" toml:"environment,omitempty"`
	Release     *string  `json:"release,omitempty" toml:"release,omitempty"`
	ServerName  *string  `json:"serverName,omitempty" toml:"server_name,omitempty"`
	Capacity    *int     `json:"capacity,omitempty" toml:"capacity,omitempty"`
	SampleRate  *float64 `json:"sampleRate,omitempty" toml:"sample_rate,omitempty"`
	IgnorePaths []string `json:"ignorePaths,omitempty" toml:"ignore_paths,omitempty"`
	Disabled    *bool    `json:"disabled,omitempty" toml:"disabled,omitempty"`
}

// CORSBlock declares the CORS policy the binary wants applied to its
// public HTTP surface. Surfaced as a structured block (not env vars)
// so lint/doctor can sanity-check it — most notably the
// wildcard-vs-credentials trap that browsers silently reject.
//
// environment_overrides typically uses this to swap origins per env:
// production locks to a specific frontend domain, preview environments
// are permissive ("*") so per-branch URLs work without manifest
// changes.
type CORSBlock struct {
	// AllowOrigins is the list of origins permitted to read responses.
	// Entries can be exact ("https://app.example.com"), a wildcard
	// ("*"), or a subdomain wildcard ("https://*.example.com").
	AllowOrigins []string `json:"allowOrigins,omitempty" toml:"allow_origins,omitempty"`

	// AllowMethods is the HTTP methods served. Defaults to GET/HEAD/
	// POST/PUT/PATCH/DELETE when empty.
	AllowMethods []string `json:"allowMethods,omitempty" toml:"allow_methods,omitempty"`

	// AllowHeaders is request headers permitted on preflighted calls.
	// Defaults to Accept/Content-Type/Authorization/X-Requested-With.
	// Set to ["*"] to echo whatever the browser asks for (dev-friendly
	// but loose).
	AllowHeaders []string `json:"allowHeaders,omitempty" toml:"allow_headers,omitempty"`

	// ExposeHeaders is response headers the browser surfaces to JS.
	// Required to surface custom headers (X-Total-Count etc.) — the
	// CORS-safelisted set is otherwise the only readable subset.
	ExposeHeaders []string `json:"exposeHeaders,omitempty" toml:"expose_headers,omitempty"`

	// AllowCredentials sets Access-Control-Allow-Credentials: true.
	// Pointer-typed so the override path distinguishes "absent"
	// (inherit) from "explicit false". Wildcard origin is incompatible
	// with credentials; the cors extension rejects that pairing.
	AllowCredentials *bool `json:"allowCredentials,omitempty" toml:"allow_credentials,omitempty"`

	// MaxAge is the preflight cache lifetime in seconds. Defaults to
	// 600 when zero. Production typically wants 86400 (24h); dev
	// wants low values so policy edits take effect immediately.
	MaxAge int `json:"maxAge,omitempty" toml:"max_age,omitempty"`

	// Disabled, when true, makes the extension a no-op for this
	// environment — useful when an upstream proxy / CDN already
	// handles CORS.
	Disabled bool `json:"disabled,omitempty" toml:"disabled,omitempty"`
}

// CORSPatch is the override-adjustable subset of CORSBlock. Slice
// fields follow list-replace semantics (matches TLS) — per-env
// origin lists are typically wholly different, not deep-merged.
// Pointers on scalars let "absent" mean inherit.
type CORSPatch struct {
	AllowOrigins     []string `json:"allowOrigins,omitempty" toml:"allow_origins,omitempty"`
	AllowMethods     []string `json:"allowMethods,omitempty" toml:"allow_methods,omitempty"`
	AllowHeaders     []string `json:"allowHeaders,omitempty" toml:"allow_headers,omitempty"`
	ExposeHeaders    []string `json:"exposeHeaders,omitempty" toml:"expose_headers,omitempty"`
	AllowCredentials *bool    `json:"allowCredentials,omitempty" toml:"allow_credentials,omitempty"`
	MaxAge           *int     `json:"maxAge,omitempty" toml:"max_age,omitempty"`
	Disabled         *bool    `json:"disabled,omitempty" toml:"disabled,omitempty"`
}

// TLSPatch is the override-adjustable subset of TLSBlock applied
// per-environment. Pointer fields let "absent" mean inherit; non-nil
// pointers replace. Domains uses a slice (not pointer) and follows
// list-replace semantics — production and staging typically declare
// fully different domain sets, so a deep-merge would only confuse.
type TLSPatch struct {
	// Domains: nil → inherit base; non-nil → fully replace. An
	// explicit empty list ([]) means "no domains in this env", which
	// is equivalent to setting Disabled=true.
	Domains  []string `json:"domains,omitempty" toml:"domains,omitempty"`
	Email    *string  `json:"email,omitempty" toml:"email,omitempty"`
	CacheDir *string  `json:"cacheDir,omitempty" toml:"cache_dir,omitempty"`
	Redirect *bool    `json:"redirect,omitempty" toml:"redirect,omitempty"`
	Staging  *bool    `json:"staging,omitempty" toml:"staging,omitempty"`
	Disabled *bool    `json:"disabled,omitempty" toml:"disabled,omitempty"`
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
	Env         map[string]string       `json:"env,omitempty" toml:"env,omitempty"`
	EnvSpecs    map[string]EnvVarPatch  `json:"envSpecs,omitempty" toml:"env_specs,omitempty"`
	SecretSpecs map[string]SecretPatch  `json:"secretSpecs,omitempty" toml:"secret_specs,omitempty"`
	FileSpecs   map[string]FilePatch    `json:"fileSpecs,omitempty" toml:"file_specs,omitempty"`
	Services    map[string]ServicePatch `json:"services,omitempty" toml:"services,omitempty"`
	// Removed holds dotted paths (e.g. "secrets.STRIPE_API_KEY",
	// "files.tls_bundle") that don't exist in this environment.
	Removed []string `json:"removed,omitempty" toml:"removed,omitempty"`
	// Hooks fully replaces the base Hooks block for this environment.
	// nil means inherit; non-nil replaces.
	Hooks *Hooks `json:"hooks,omitempty" toml:"hooks,omitempty"`

	// TLS patches the base TLSBlock. Field-by-field merge — Domains
	// is a list-replace, everything else is pointer-set. nil means
	// "inherit base TLS unchanged". Operators typically use this to
	// swap domains (production app.example.com vs staging.example.com)
	// and to flip Staging=true in non-production environments.
	TLS *TLSPatch `json:"tls,omitempty" toml:"tls,omitempty"`

	// CORS patches the base CORSBlock. Same merge shape as TLS —
	// slice fields list-replace, scalars pointer-set. Typical use
	// is locking AllowOrigins to a per-environment frontend URL.
	CORS *CORSPatch `json:"cors,omitempty" toml:"cors,omitempty"`

	// Errors patches the base ErrorsBlock. Common override is
	// SampleRate (cut preview noise) or Disabled (silence a
	// specific environment).
	Errors *ErrorsPatch `json:"errors,omitempty" toml:"errors,omitempty"`
}

// EnvVarPatch is the subset of EnvVar fields an override is allowed to
// adjust. Pointer fields let "absent" mean "inherit", while non-nil
// pointer means "set". Default and EnvScoped and Validation can all be
// adjusted; Name and Source cannot.
type EnvVarPatch struct {
	Description *string        `json:"description,omitempty" toml:"description,omitempty"`
	Required    *bool          `json:"required,omitempty" toml:"required,omitempty"`
	Default     *string        `json:"default,omitempty" toml:"default,omitempty"`
	EnvScoped   *bool          `json:"envScoped,omitempty" toml:"env_scoped,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" toml:"validation,omitempty"`
}

// SecretPatch is the override-adjustable subset of Secret.
type SecretPatch struct {
	Description *string        `json:"description,omitempty" toml:"description,omitempty"`
	Required    *bool          `json:"required,omitempty" toml:"required,omitempty"`
	EnvScoped   *bool          `json:"envScoped,omitempty" toml:"env_scoped,omitempty"`
	RotateAlert *string        `json:"rotateAlert,omitempty" toml:"rotate_alert,omitempty"`
	Validation  *EnvValidation `json:"validation,omitempty" toml:"validation,omitempty"`
}

// FilePatch is the override-adjustable subset of File.
type FilePatch struct {
	Description *string `json:"description,omitempty" toml:"description,omitempty"`
	Mode        *int    `json:"mode,omitempty" toml:"mode,omitempty"`
	Secret      *bool   `json:"secret,omitempty" toml:"secret,omitempty"`
	EnvScoped   *bool   `json:"envScoped,omitempty" toml:"env_scoped,omitempty"`
}

// ServicePatch is the override-adjustable subset of ServiceNeed. Most
// useful for sizing/backup tweaks per environment (large in prod,
// small in preview) and for adding new ExposeAs entries.
type ServicePatch struct {
	Version  *string           `json:"version,omitempty" toml:"version,omitempty"`
	Size     *string           `json:"size,omitempty" toml:"size,omitempty"`
	Backup   *string           `json:"backup,omitempty" toml:"backup,omitempty"`
	ExposeAs map[string]string `json:"exposeAs,omitempty" toml:"expose_as,omitempty"`
	// Ephemeral=true tells the platform to tear down the provisioned
	// resource when the environment is destroyed (typical for preview
	// envs where you don't want orphaned databases lying around).
	Ephemeral *bool `json:"ephemeral,omitempty" toml:"ephemeral,omitempty"`
	// Optional flips a service to non-required for this environment
	// (e.g. payment service in preview).
	Optional *bool `json:"optional,omitempty" toml:"optional,omitempty"`
}