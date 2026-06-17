package client

import (
	"sort"
	"strings"

	"github.com/paulmanoni/nexus/registry"
)

// SchemaVersion is the contract version emitted in Manifest.Version.
// SDK consumers gate on the major: a "client.v1" manifest is
// guaranteed to be readable by any v1-aware consumer. Additive
// changes (a new EndpointInfo field) leave the version untouched;
// field removal or shape change bumps the major.
const SchemaVersion = "client.v1"

// Manifest is the SDK-tailored shape served at
// GET <path>/manifest.json. Distinct from manifest.Manifest (the
// deploy-time, admin-token-gated document): public, type-rich, and
// scoped to what an SDK consumer needs to call into the app.
type Manifest struct {
	Version   string                        `json:"version"`
	BasePath  string                        `json:"basePath"`
	Services  []ServiceInfo                 `json:"services,omitempty"`
	Endpoints []EndpointInfo                `json:"endpoints,omitempty"`
	Resources []ResourceInfo                `json:"resources,omitempty"`
	WS        []WSPathInfo                  `json:"ws,omitempty"`
	Auth      *AuthInfo                     `json:"auth,omitempty"`
	Refs      map[string]registry.NamedType `json:"refs,omitempty"`

	// Projected marks the stripped (non-Public) manifest served to
	// anonymous browsers — auth flows only, no schemas, no GraphQL/CRUD
	// ops. The runtime SDK reads it to turn the otherwise-cryptic
	// "no op named X" miss into a "the server is serving the stripped
	// manifest" hint. Omitted (false) on the full manifest.
	Projected bool `json:"projected,omitempty"`
}

// ServiceInfo is the SDK-friendly view of a service. Drops topology
// fields (ResourceDeps, ServiceDeps, Remote) the SDK doesn't need.
type ServiceInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// EndpointInfo is one callable surface — a REST route, a GraphQL
// op, or a WebSocket message handler.
type EndpointInfo struct {
	Service     string            `json:"service"`
	Module      string            `json:"module,omitempty"`
	Transport   string            `json:"transport"` // "rest" | "graphql" | "websocket"
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Args        *registry.TypeRef `json:"args,omitempty"`
	Return      *registry.TypeRef `json:"return,omitempty"`

	// AuthRequired is true when the endpoint is gated by auth.Required
	// (or any middleware whose name starts with "auth:" — covers
	// Required + Requires).
	AuthRequired bool `json:"authRequired,omitempty"`

	// RequiresPerm is the list of permission strings auth.Requires
	// declared. Empty for unauthed endpoints or auth.Required without
	// specific perms. Derived from middleware names of the form
	// "auth:requires:perm-a,perm-b".
	RequiresPerm []string `json:"requiresPerm,omitempty"`

	// AuthFlow is "login" | "logout" | "me" when the endpoint was
	// marked via nexus.AuthRoute. Empty for normal endpoints.
	AuthFlow string `json:"authFlow,omitempty"`

	Deprecated        bool   `json:"deprecated,omitempty"`
	DeprecationReason string `json:"deprecationReason,omitempty"`
}

// ResourceInfo is the SDK view of a resource — light enough to
// drive a "where does this go?" picker in a UI without leaking
// orchestration details.
type ResourceInfo struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
}

// WSPathInfo groups every typed AsWS message under one WebSocket
// path so the SDK can build a single connection handle per path
// with a typed dispatch table.
type WSPathInfo struct {
	Path     string      `json:"path"`
	Service  string      `json:"service"`
	Messages []WSMessage `json:"messages"`
}

// WSMessage is one (msgType → args) entry on a WS path.
type WSMessage struct {
	Type        string            `json:"type"`
	Args        *registry.TypeRef `json:"args,omitempty"`
	Description string            `json:"description,omitempty"`
}

// ExtractorInfo describes how the running auth module pulls tokens
// off requests. Mirrors auth.ExtractorInfo (the canonical
// definition) — duplicated here so the SDK manifest stays
// auth-package-independent and avoids the nexus → client → auth
// import cycle. nexus.New is responsible for translating from
// auth.ExtractorInfo via the AuthInfo callback on client.Config.
//
// Strategy values match auth.ExtractorInfo.Strategy:
//
//	"bearer"  HeaderName populated; SDK sends "Authorization: Bearer"
//	"cookie"  CookieName populated; SDK sets credentials: 'include'
//	"apikey"  HeaderName populated; SDK sends a bare header value
//	"chain"   Chain populated with the underlying strategies
//	"custom"  user-supplied extractor; SDK falls back to credentials
type ExtractorInfo struct {
	Strategy   string          `json:"strategy"`
	HeaderName string          `json:"headerName,omitempty"`
	CookieName string          `json:"cookieName,omitempty"`
	Chain      []ExtractorInfo `json:"chain,omitempty"`
}

// AuthInfo describes the running auth surface. The extractor shape
// tells the SDK where to put the access token; LoginPath /
// LogoutPath / MePath (plus their *Transport / *Name siblings) are
// discovered from endpoints marked via nexus.AuthRoute.
//
// Transport is "rest" or "graphql" — the SDK switches between
// rest() and _gql() dispatch based on this. Name is the GraphQL op
// name when Transport == "graphql"; empty for REST. The legacy
// *Path fields stay populated for both transports (the GraphQL
// mount path) so older SDK builds continue to resolve auth flows.
type AuthInfo struct {
	ExtractorInfo
	// TokenField, when set, names where the access token lives in a
	// login response — a dotted path like "token", "accessToken", or
	// "data.token". The SDK reads it instead of guessing common shapes
	// (see extractLoginToken in nexus-client.js). Empty by default; the
	// framework leaves it unset so apps/extensions populate it via a
	// custom client.Config.Manifest projection when their login handler
	// uses a non-standard envelope.
	TokenField string `json:"tokenField,omitempty"`
	// CSRFCookie / CSRFHeader configure the double-submit CSRF defense
	// the SDK applies to state-changing requests under cookie-based auth
	// strategies (cookie/chain/custom). Empty falls back to the SDK
	// defaults ("csrftoken" / "X-CSRFToken", the Django/Laravel
	// convention; see DefaultCSRFCookie / DefaultCSRFHeader). auth.Module
	// populates these from auth.Config (defaulted via AuthMeta.WithDefaults)
	// so a wired app carries them explicitly.
	CSRFCookie      string `json:"csrfCookie,omitempty"`
	CSRFHeader      string `json:"csrfHeader,omitempty"`
	LoginPath       string `json:"loginPath,omitempty"`
	LoginTransport  string `json:"loginTransport,omitempty"`
	LoginName       string `json:"loginName,omitempty"`
	LogoutPath      string `json:"logoutPath,omitempty"`
	LogoutTransport string `json:"logoutTransport,omitempty"`
	LogoutName      string `json:"logoutName,omitempty"`
	MePath          string `json:"mePath,omitempty"`
	MeTransport     string `json:"meTransport,omitempty"`
	MeName          string `json:"meName,omitempty"`
}

// relPath strips the deployment-wide basePath from a stored (absolute)
// endpoint path so the manifest carries paths RELATIVE to BasePath. Every SDK
// consumer (the runtime client, the codegen'd client, the WS handle) composes
// the request URL as origin + basePath + path; storing absolute paths while
// also setting BasePath would double the prefix when route_prefix is set.
//
// A no-op when basePath is empty (the common case) or when path doesn't start
// with it (defensive — all user paths go through app.PrefixPath, so they do).
func relPath(path, basePath string) string {
	if basePath == "" || !strings.HasPrefix(path, basePath) {
		return path
	}
	rel := path[len(basePath):]
	if rel == "" {
		return "/"
	}
	return rel
}

// buildManifest projects the registry into the SDK manifest shape.
// reads:
//   - reg.Services()    → ServiceInfo[]
//   - reg.Endpoints()   → EndpointInfo[] (with auth flags + WS regrouping)
//   - reg.Resources()   → ResourceInfo[] (drops health/details/depends)
//   - authInfo          → AuthInfo.ExtractorInfo (when callback is non-nil)
//   - schemaRefs        → Manifest.Refs (named-type pool)
//
// basePath is the deployment-wide route prefix (app.routePrefix).
// Stamped onto Manifest.BasePath so the SDK can prepend it to every
// REST / WS path it calls — avoiding hardcoded paths in the JS
// runtime.
//
// authInfo is a callback to keep client/ from importing auth/ (the
// cycle would be nexus → client → auth → nexus). nexus.New is
// responsible for translating *auth.Manager.Info() into the shape
// callback returns.
func buildManifest(reg *registry.Registry, authInfo func() ExtractorInfo, schemaRefs map[string]registry.NamedType, basePath string) Manifest {
	m := Manifest{
		Version:  SchemaVersion,
		BasePath: basePath,
	}
	for _, s := range reg.Services() {
		m.Services = append(m.Services, ServiceInfo{Name: s.Name, Description: s.Description})
	}

	wsByPath := map[string]*WSPathInfo{}
	for _, e := range reg.Endpoints() {
		// Stored endpoint paths are absolute — app.PrefixPath baked the
		// deployment-wide route prefix into them at registration. The
		// manifest carries paths RELATIVE to BasePath, since every SDK
		// consumer composes the URL as origin + basePath + path; emitting
		// absolute paths here would double the prefix when route_prefix is
		// set (e.g. /api + /api/x). relPath strips it back out.
		relP := relPath(e.Path, basePath)
		if e.Transport == registry.WebSocket {
			grp, ok := wsByPath[relP]
			if !ok {
				grp = &WSPathInfo{Path: relP, Service: e.Service}
				wsByPath[relP] = grp
			}
			grp.Messages = append(grp.Messages, WSMessage{
				Type:        e.Method,
				Args:        e.ArgsSchema,
				Description: e.Description,
			})
			// WS endpoints still get a top-level EndpointInfo so a
			// caller that wants the flat list can read them; the
			// grouped WSPathInfo is the convenience view for SDKs
			// building one connection handle per path.
		}
		info := EndpointInfo{
			Service:           e.Service,
			Module:            e.Module,
			Transport:         string(e.Transport),
			Method:            e.Method,
			Path:              relP,
			Name:              e.Name,
			Description:       e.Description,
			Args:              e.ArgsSchema,
			Return:            e.ReturnSchema,
			Deprecated:        e.Deprecated,
			DeprecationReason: e.DeprecationReason,
		}
		info.AuthRequired, info.RequiresPerm = deriveAuth(e.Middleware)
		if e.Tags != nil {
			info.AuthFlow = e.Tags[authFlowTagKey]
		}
		m.Endpoints = append(m.Endpoints, info)
	}
	// Stable order — sorted by service then transport then path so a
	// regenerated manifest produces byte-equivalent output for
	// snapshot tests.
	sort.SliceStable(m.Endpoints, func(i, j int) bool {
		a, b := m.Endpoints[i], m.Endpoints[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Transport != b.Transport {
			return a.Transport < b.Transport
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})

	for _, grp := range wsByPath {
		sort.Slice(grp.Messages, func(i, j int) bool { return grp.Messages[i].Type < grp.Messages[j].Type })
		m.WS = append(m.WS, *grp)
	}
	sort.Slice(m.WS, func(i, j int) bool { return m.WS[i].Path < m.WS[j].Path })

	for _, r := range reg.Resources() {
		m.Resources = append(m.Resources, ResourceInfo{
			Name:        r.Name,
			Kind:        string(r.Kind),
			Description: r.Description,
		})
	}

	if authInfo != nil {
		ai := &AuthInfo{ExtractorInfo: authInfo()}
		// Discover login/logout/me from AuthRoute-tagged endpoints.
		// For GraphQL ops, also record the op name so the SDK can
		// dispatch via _gql; the path field carries the mount URL.
		for _, e := range m.Endpoints {
			name := ""
			if e.Transport == string(registry.GraphQL) {
				name = e.Name
			}
			switch e.AuthFlow {
			case "login":
				ai.LoginPath = e.Path
				ai.LoginTransport = e.Transport
				ai.LoginName = name
			case "logout":
				ai.LogoutPath = e.Path
				ai.LogoutTransport = e.Transport
				ai.LogoutName = name
			case "me":
				ai.MePath = e.Path
				ai.MeTransport = e.Transport
				ai.MeName = name
			}
		}
		m.Auth = ai
	}

	if len(schemaRefs) > 0 {
		m.Refs = make(map[string]registry.NamedType, len(schemaRefs))
		for k, v := range schemaRefs {
			m.Refs[k] = v
		}
	}
	return m
}

// authFlowTagKey is the Tags key nexus.AuthRoute uses. Mirrored
// here as a string literal to avoid the client/ → nexus/ import
// cycle (nexus imports client/ for the Mount wiring; client/
// reads tags from the registry without naming nexus types).
const authFlowTagKey = "auth.flow"

// deriveAuth reads middleware names and returns (requiresIdentity,
// listOfRequiredPerms). Middleware shape conventions:
//
//	"auth:required"                 → requires identity, no perms
//	"auth:requires"                 → requires identity, no perms
//	"auth:requires:perm-a,perm-b"   → requires identity + listed perms
//	"auth:optional"                 → does NOT require identity
//
// Other middleware names are ignored. Non-auth middleware passes
// through untouched.
func deriveAuth(middleware []string) (bool, []string) {
	required := false
	var perms []string
	for _, name := range middleware {
		switch {
		case name == "auth:optional":
			// Reads identity but doesn't enforce — explicitly NOT
			// required. Don't flip the flag.
		case strings.HasPrefix(name, "auth:requires:"):
			required = true
			tail := strings.TrimPrefix(name, "auth:requires:")
			for _, p := range strings.Split(tail, ",") {
				if p = strings.TrimSpace(p); p != "" {
					perms = append(perms, p)
				}
			}
		case name == "auth:requires" || name == "auth:required" || strings.HasPrefix(name, "auth:"):
			required = true
		}
	}
	if len(perms) > 1 {
		sort.Strings(perms)
	}
	return required, perms
}
