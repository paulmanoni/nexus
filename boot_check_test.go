package nexus

import (
	"testing"

	"github.com/paulmanoni/nexus/manifest"
)

// TestCollectBootIssues verifies the boot self-check merges config-file issues
// with registered topology checks, and drains the one-shot pending list so a
// second in-process Run doesn't double-report.
func TestCollectBootIssues(t *testing.T) {
	savedChecks, savedPending := bootChecks, pendingBootIssues
	t.Cleanup(func() { bootChecks, pendingBootIssues = savedChecks, savedPending })
	bootChecks, pendingBootIssues = nil, nil

	addPendingBootIssues([]manifest.Issue{
		{Severity: manifest.SeverityWarning, Path: "runtime.cors", Message: "config warn"},
	})
	RegisterBootCheck(func() []manifest.Issue {
		return []manifest.Issue{{Severity: manifest.SeverityError, Path: "pubsub", Message: "topo err"}}
	})
	// A nil-returning check must contribute nothing (and not panic).
	RegisterBootCheck(func() []manifest.Issue { return nil })

	got := collectBootIssues()
	if len(got) != 2 {
		t.Fatalf("first collect: want 2 issues (1 pending + 1 topology), got %d: %+v", len(got), got)
	}
	if len(pendingBootIssues) != 0 {
		t.Errorf("pending issues not drained after collect: %+v", pendingBootIssues)
	}

	// Second collect: pending is drained, but registered topology checks still run.
	got2 := collectBootIssues()
	if len(got2) != 1 {
		t.Fatalf("second collect: want 1 (topology only, pending drained), got %d: %+v", len(got2), got2)
	}
	if got2[0].Path != "pubsub" {
		t.Errorf("second collect should be the topology check, got path %q", got2[0].Path)
	}
}

// TestRegisterBootCheck_NilIgnored ensures a nil check can't be registered
// (guards the range in collectBootIssues from a nil call).
func TestRegisterBootCheck_NilIgnored(t *testing.T) {
	saved := bootChecks
	t.Cleanup(func() { bootChecks = saved })
	bootChecks = nil

	RegisterBootCheck(nil)
	if len(bootChecks) != 0 {
		t.Fatalf("nil check should be ignored, got %d registered", len(bootChecks))
	}
}
