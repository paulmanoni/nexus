package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	nexusmanifest "github.com/paulmanoni/nexus/manifest"
)

// routesOptions captures every flag `nexus routes` accepts. Input
// sources mirror lint / doctor so an operator's muscle memory
// carries across the three commands.
type routesOptions struct {
	filePath    string
	binaryPath  string
	inputFormat string // "" | "yaml" | "json"

	// Output format. JSON passes the unfiltered Manifest.Routes
	// (post any --filter narrowing) verbatim, so machine consumers
	// get the canonical shape.
	jsonOut bool

	// Filters narrow the rendered set. Empty filter = pass through.
	kindFilter       string // "rest" | "graphql.query" | "graphql.mutation" | "graphql.subscription" | "ws"
	moduleFilter     string
	methodFilter     string // case-insensitive
	pathFilter       string // substring match
	deploymentFilter string
	authFilter       string // "none" | "optional" | "required"
}

// newRoutesCmd wires `nexus routes` — list every HTTP/GraphQL/WS
// endpoint a binary will mount, without booting it. The data
// already lives in the manifest (NEXUS_PRINT_MANIFEST=1 emits it);
// this command renders it as a sorted, filterable table.
//
// Useful for:
//   - API audits ("does this binary still expose /admin/reset?")
//   - CI diff checks (compare today's routes vs main's)
//   - Onboarding ("what does this app serve?")
//   - Pre-deploy smoke ("does the new build expose what we expect?")
//
// Input sources (same as lint / doctor):
//
//	nexus routes <manifest.json>      JSON manifest
//	nexus routes <nexus.toml>  YAML manifest (auto-detected)
//	nexus routes -                    stdin
//	nexus routes --binary=PATH        exec binary in print mode
//
// Filters narrow the output; all are AND-combined:
//
//	--kind rest                  only REST routes
//	--module users               only routes owned by the users module
//	--method GET                 case-insensitive method match
//	--path /users                substring match on the URL path
//	--deployment users-svc       only routes in this deployment
//	--auth required              only auth-required routes
//
// Exit codes: 0 on success; 1 on input / parse errors. Empty output
// (no routes match the filters) is success — operators piping into
// `wc -l` shouldn't get false alarms.
func newRoutesCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts routesOptions

	cmd := &cobra.Command{
		Use:           "routes [manifest]",
		Short:         "List the HTTP/GraphQL/WS endpoints a nexus binary will mount",
		SilenceErrors: true,
		Long: `Render every route a nexus binary registers as a sorted table.

The route table is sourced from the binary's NEXUS_PRINT_MANIFEST=1 output,
or any file containing a manifest (JSON / YAML, auto-detected by extension).
No live app needed — `+"`nexus routes`"+` is a static analysis of the manifest.

Input sources:
  nexus routes <manifest.json>      JSON manifest file
  nexus routes <nexus.toml>  YAML manifest (auto-detected)
  nexus routes -                    read from stdin
  nexus routes --binary=PATH        exec binary in NEXUS_PRINT_MANIFEST=1

Filters (AND-combined):
  --kind rest|graphql.query|graphql.mutation|graphql.subscription|ws
  --module <name>
  --method GET|POST|...           (case-insensitive)
  --path /users                   (substring match)
  --deployment <name>
  --auth none|optional|required

Output: text table by default; --json passes through the filtered
Manifest.Routes slice verbatim for machine consumers.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.filePath = args[0]
			}
			return runRoutes(stdout, stderr, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.jsonOut, "json", false, "emit routes as a JSON array instead of the text table")
	cmd.Flags().StringVar(&opts.binaryPath, "binary", "", "exec the binary in NEXUS_PRINT_MANIFEST=1 mode and list its routes")
	cmd.Flags().StringVar(&opts.kindFilter, "kind", "", "only routes of this kind (rest, graphql.query, graphql.mutation, graphql.subscription, ws)")
	cmd.Flags().StringVar(&opts.moduleFilter, "module", "", "only routes owned by this module")
	cmd.Flags().StringVar(&opts.methodFilter, "method", "", "only routes with this HTTP method (case-insensitive)")
	cmd.Flags().StringVar(&opts.pathFilter, "path", "", "only routes whose path contains this substring")
	cmd.Flags().StringVar(&opts.deploymentFilter, "deployment", "", "only routes in this deployment")
	cmd.Flags().StringVar(&opts.authFilter, "auth", "", "only routes with this auth setting (none, optional, required)")

	var yamlIn, jsonIn bool
	cmd.Flags().BoolVar(&yamlIn, "yaml", false, "force YAML input parsing")
	cmd.Flags().BoolVar(&jsonIn, "json-in", false, "force JSON input parsing")
	cmd.PreRunE = func(_ *cobra.Command, _ []string) error {
		if yamlIn && jsonIn {
			return errors.New("nexus routes: --yaml and --json-in are mutually exclusive")
		}
		switch {
		case yamlIn:
			opts.inputFormat = "yaml"
		case jsonIn:
			opts.inputFormat = "json"
		}
		return nil
	}

	return cmd
}

// runRoutes is the testable entry point. Resolves input via lint's
// helpers (readManifestSource + parseManifest + resolveInputFormat)
// so the three CLI commands stay in lockstep.
func runRoutes(stdout, stderr io.Writer, opts routesOptions) error {
	if opts.filePath != "" && opts.binaryPath != "" {
		return errors.New("nexus routes: cannot combine a manifest path with --binary")
	}
	if opts.inputFormat == "yaml" && opts.binaryPath != "" {
		return errors.New("nexus routes: --yaml is incompatible with --binary (binary print mode emits JSON)")
	}

	lintOpts := lintOptions{
		filePath:    opts.filePath,
		binaryPath:  opts.binaryPath,
		inputFormat: opts.inputFormat,
	}
	raw, source, err := readManifestSource(lintOpts)
	if err != nil {
		return remapLintErrorTag(err, "routes")
	}
	m, err := parseManifest(raw, resolveInputFormat(lintOpts, source), source)
	if err != nil {
		return remapLintErrorTag(err, "routes")
	}

	routes := collectRoutes(m)
	routes = applyFilters(routes, opts)
	sortRoutes(routes)

	if opts.jsonOut {
		return emitRoutesJSON(stdout, routes)
	}
	return emitRoutesTable(stdout, source, routes)
}

// collectRoutes returns the v1 Routes slice when populated; falls
// back to projecting Endpoints (v0 back-compat) into Route shape so
// older manifests still produce a useful table. New users only need
// the Routes path; the fallback exists for older binaries that
// haven't re-emitted their manifest since v0.20-ish.
func collectRoutes(m nexusmanifest.Manifest) []nexusmanifest.Route {
	if len(m.Routes) > 0 {
		out := make([]nexusmanifest.Route, len(m.Routes))
		copy(out, m.Routes)
		return out
	}
	if len(m.Endpoints) == 0 {
		return nil
	}
	out := make([]nexusmanifest.Route, 0, len(m.Endpoints))
	for _, e := range m.Endpoints {
		out = append(out, nexusmanifest.Route{
			Kind:   normalizeTransport(e.Transport),
			Method: e.Method,
			Path:   e.Path,
			Module: e.Service,
		})
	}
	return out
}

// normalizeTransport translates EndpointSummary.Transport strings
// into the v1 Route.Kind taxonomy. Best-effort: known transports
// map directly; unknown ones pass through so the table still shows
// SOMETHING in the Kind column rather than a confusing blank.
func normalizeTransport(t string) string {
	switch strings.ToLower(t) {
	case "rest", "http":
		return "rest"
	case "graphql", "graphql.query":
		return "graphql.query"
	case "ws", "websocket":
		return "ws"
	default:
		return t
	}
}

// applyFilters narrows routes per the operator's flags. Each filter
// is an early-skip; an empty filter passes everything through.
func applyFilters(routes []nexusmanifest.Route, opts routesOptions) []nexusmanifest.Route {
	if opts.kindFilter == "" && opts.moduleFilter == "" && opts.methodFilter == "" &&
		opts.pathFilter == "" && opts.deploymentFilter == "" && opts.authFilter == "" {
		return routes
	}
	methodLower := strings.ToUpper(strings.TrimSpace(opts.methodFilter))
	out := routes[:0]
	for _, r := range routes {
		if opts.kindFilter != "" && r.Kind != opts.kindFilter {
			continue
		}
		if opts.moduleFilter != "" && r.Module != opts.moduleFilter {
			continue
		}
		if methodLower != "" && strings.ToUpper(r.Method) != methodLower {
			continue
		}
		if opts.pathFilter != "" && !strings.Contains(r.Path, opts.pathFilter) && !strings.Contains(r.Operation, opts.pathFilter) {
			continue
		}
		if opts.deploymentFilter != "" && r.Deployment != opts.deploymentFilter {
			continue
		}
		if opts.authFilter != "" && r.Auth != opts.authFilter {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortRoutes orders by kind → method → path/operation so similar
// surfaces group together: all REST GETs, then POSTs, then GraphQL
// queries, then mutations, then WebSockets. Within each group,
// alphabetical by path so the same manifest renders identically
// across runs.
func sortRoutes(routes []nexusmanifest.Route) {
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Kind != routes[j].Kind {
			return kindRank(routes[i].Kind) < kindRank(routes[j].Kind)
		}
		if routes[i].Method != routes[j].Method {
			return methodRank(routes[i].Method) < methodRank(routes[j].Method)
		}
		ai := routes[i].Path
		if ai == "" {
			ai = routes[i].Operation
		}
		bj := routes[j].Path
		if bj == "" {
			bj = routes[j].Operation
		}
		return ai < bj
	})
}

// kindRank orders kinds so the table reads top-to-bottom in
// decreasing operator familiarity: REST first (most common), then
// GraphQL ops, then WebSockets.
func kindRank(k string) int {
	switch k {
	case "rest":
		return 0
	case "graphql.query":
		return 1
	case "graphql.mutation":
		return 2
	case "graphql.subscription":
		return 3
	case "ws":
		return 4
	default:
		return 5
	}
}

// methodRank orders HTTP methods by typical CRUD ordering — GETs
// first (read), then writes. Reads cluster at the top so the
// "what data does this expose?" question is answered without
// scrolling.
func methodRank(m string) int {
	switch strings.ToUpper(m) {
	case "GET":
		return 0
	case "POST":
		return 1
	case "PUT":
		return 2
	case "PATCH":
		return 3
	case "DELETE":
		return 4
	default:
		return 5
	}
}

// emitRoutesJSON writes the filtered routes as a JSON array, indent
// 2. Machine consumers should branch on the array length, not the
// presence of any "empty" sentinel.
func emitRoutesJSON(stdout io.Writer, routes []nexusmanifest.Route) error {
	if routes == nil {
		routes = []nexusmanifest.Route{}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(routes)
}

// emitRoutesTable renders an aligned tab-separated table:
//
//	KIND   METHOD  PATH/OPERATION    MODULE    DEPLOYMENT  AUTH
//	rest   GET     /users/:id        users                 required
//	rest   POST    /checkout         checkout              none
//	gql    -       listAdverts       adverts               optional
//	ws     -       /events           chat                  required
//
// Empty MODULE / DEPLOYMENT / AUTH columns render as "-" so columns
// stay aligned and missing values are visually obvious.
func emitRoutesTable(stdout io.Writer, source string, routes []nexusmanifest.Route) error {
	if len(routes) == 0 {
		fmt.Fprintf(stdout, "nexus routes: %s — no routes matched\n", source)
		return nil
	}

	fmt.Fprintf(stdout, "nexus routes: %s (%d route%s)\n\n", source, len(routes), pluralize("", len(routes)))

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tMETHOD\tPATH / OPERATION\tMODULE\tDEPLOYMENT\tAUTH")
	for _, r := range routes {
		target := r.Path
		if target == "" {
			target = r.Operation
		}
		method := r.Method
		if method == "" {
			method = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			emptyDash(r.Kind),
			method,
			emptyDash(target),
			emptyDash(r.Module),
			emptyDash(r.Deployment),
			emptyDash(authLabel(r.Auth)),
		)
	}
	return w.Flush()
}

// emptyDash renders empty strings as "-" so columns stay aligned
// and missing data reads clearly.
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// authLabel normalizes Route.Auth for display. Empty string in the
// manifest typically means "none" by convention; render that
// explicitly so the operator doesn't have to remember the default.
func authLabel(a string) string {
	if a == "" {
		return "none"
	}
	return a
}
