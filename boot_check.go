package nexus

import (
	"fmt"
	"os"
	"sort"

	"github.com/paulmanoni/nexus/manifest"
)

// BootCheck is a boot-time self-check: it inspects live app topology after
// wiring and returns any foot-guns as manifest.Issues. It exists to promote
// failures that would otherwise only surface at runtime — e.g. pubsub's "topic
// has no transport bound", which today fails at the first Publish (possibly at
// 2am) — into a loud, early report at boot in dev, so a junior sees the problem
// before deploy. See ERRORS.md.
type BootCheck func() []manifest.Issue

var (
	bootChecks        []BootCheck
	pendingBootIssues []manifest.Issue // config-file lint issues collected during load
)

// RegisterBootCheck adds a boot-time self-check. Call it from an init() in the
// package that owns the invariant — the same pay-for-what-you-use pattern as
// dashboard.RegisterSnapshotExtra: the check only exists if the app imports
// that package, so the framework core stays free of the dependency. Safe to
// call before Run.
func RegisterBootCheck(fn BootCheck) {
	if fn != nil {
		bootChecks = append(bootChecks, fn)
	}
}

// addPendingBootIssues stashes issues discovered before the DI graph exists
// (e.g. the nexus.toml lint in autoLoad) so runBootChecks can report them
// alongside the topology checks in one block.
func addPendingBootIssues(issues []manifest.Issue) {
	pendingBootIssues = append(pendingBootIssues, issues...)
}

// collectBootIssues gathers the config-file issues plus every registered
// topology check's findings. Pure and drainable — separated from reporting so
// tests can assert on the issues without capturing stderr.
func collectBootIssues() []manifest.Issue {
	issues := append([]manifest.Issue(nil), pendingBootIssues...)
	for _, c := range bootChecks {
		issues = append(issues, c()...)
	}
	pendingBootIssues = nil // drain so a second Run in-process doesn't double-report
	return issues
}

// runBootChecks collects and reports boot self-check issues. Called from a DI
// invoke that Run appends LAST (so it runs after pubsub's BindTopics and every
// other wiring invoke) and only in dev — so production pays nothing and a clean
// app prints nothing. Advisory: it never aborts boot (genuine fatal misconfig
// already fails elsewhere); the value is EARLY VISIBILITY.
func runBootChecks() { reportBootIssues(collectBootIssues()) }

// reportBootIssues prints issues to stderr, errors first, deep-linkable by path.
func reportBootIssues(issues []manifest.Issue) {
	if len(issues) == 0 {
		return
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity == manifest.SeverityError // errors before warnings
		}
		return issues[i].Path < issues[j].Path
	})
	fmt.Fprintf(os.Stderr,
		"\nnexus: boot self-check found %d issue(s) — dev-only, fix before deploy (see ERRORS.md):\n",
		len(issues))
	for _, is := range issues {
		fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", is.Severity, is.Path, is.Message)
	}
	fmt.Fprintln(os.Stderr)
}
