package pubsub

import "testing"

// TestBootCheck_NoTransport asserts the boot self-check flags declared topics
// with no bound transport, and stays quiet once a transport is bound (or when
// there are no topics at all).
func TestBootCheck_NoTransport(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	// No topics → no issue.
	if got := bootCheck(); len(got) != 0 {
		t.Fatalf("no topics: want 0 issues, got %d: %+v", len(got), got)
	}

	// Declare a topic, bind nothing → one error naming the fix.
	_ = NewTopic[testEvent]("bc-orders", TopicConfig{})
	got := bootCheck()
	if len(got) != 1 {
		t.Fatalf("topic + no transport: want 1 issue, got %d: %+v", len(got), got)
	}
	if got[0].Severity != "error" || got[0].Path != "pubsub" {
		t.Errorf("unexpected issue shape: %+v", got[0])
	}

	// Bind a transport → issue clears.
	BindTopics(NewInMemoryTransport())
	if got := bootCheck(); len(got) != 0 {
		t.Fatalf("after BindTopics: want 0 issues, got %d: %+v", len(got), got)
	}
}
