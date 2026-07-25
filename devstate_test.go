package nexus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// counterStore is a stand-in for the in-memory stores this feature exists
// for: a map behind a mutex that would otherwise die with the process.
type counterStore struct {
	mu     sync.Mutex
	counts map[string]int
	failOn string // "snapshot" / "restore" to exercise the error paths
}

func newCounterStore() *counterStore {
	return &counterStore{counts: map[string]int{}}
}

func (c *counterStore) add(k string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[k] += n
}

func (c *counterStore) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[k]
}

func (c *counterStore) SnapshotDev() ([]byte, error) {
	if c.failOn == "snapshot" {
		return nil, errors.New("boom")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Marshal(c.counts)
}

func (c *counterStore) RestoreDev(b []byte) error {
	if c.failOn == "restore" {
		return errors.New("boom")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return json.Unmarshal(b, &c.counts)
}

// devSession points the dev-state machinery at a temp file for the duration
// of a test, and hands back a func that simulates a rebuild: everything
// registered so far is snapshotted and the registry is emptied, exactly as it
// would be in the process that replaces this one.
func devSession(t *testing.T) (restart func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	t.Setenv(devStateEnv, path)
	saved := devStates
	devStates = &devStateRegistry{}
	t.Cleanup(func() { devStates = saved })
	return func() {
		t.Helper()
		if err := devStates.writeDevState(); err != nil {
			t.Fatalf("writeDevState: %v", err)
		}
		devStates = &devStateRegistry{}
	}
}

func TestPreserveDevCarriesStateAcrossRestart(t *testing.T) {
	restart := devSession(t)

	before := newCounterStore()
	PreserveDev("counters", before)
	before.add("visits", 3)

	restart()

	after := newCounterStore()
	PreserveDev("counters", after)
	if got := after.get("visits"); got != 3 {
		t.Errorf("visits = %d after restart, want 3", got)
	}

	// And it keeps carrying: the second generation's writes survive too.
	after.add("visits", 1)
	restart()
	third := newCounterStore()
	PreserveDev("counters", third)
	if got := third.get("visits"); got != 4 {
		t.Errorf("visits = %d after second restart, want 4", got)
	}
}

func TestPreserveDevIsolatesNames(t *testing.T) {
	restart := devSession(t)
	a, b := newCounterStore(), newCounterStore()
	PreserveDev("a", a)
	PreserveDev("b", b)
	a.add("x", 1)
	b.add("x", 9)

	restart()

	a2, b2 := newCounterStore(), newCounterStore()
	PreserveDev("a", a2)
	PreserveDev("b", b2)
	if a2.get("x") != 1 || b2.get("x") != 9 {
		t.Errorf("state crossed names: a=%d b=%d, want 1 and 9", a2.get("x"), b2.get("x"))
	}

	// A name nobody registered this time is simply not restored — and must
	// not upset the ones that are.
	restart()
	a3 := newCounterStore()
	PreserveDev("a", a3)
	if a3.get("x") != 1 {
		t.Errorf("a = %d, want 1", a3.get("x"))
	}
}

// Outside `nexus dev` the whole thing is inert: nothing registered, nothing
// written, no file to leak into a production deployment.
func TestPreserveDevNoopOutsideDev(t *testing.T) {
	t.Setenv(devStateEnv, "")
	saved := devStates
	devStates = &devStateRegistry{}
	defer func() { devStates = saved }()

	s := newCounterStore()
	s.add("x", 1)
	PreserveDev("counters", s)
	if len(devStates.current) != 0 {
		t.Errorf("registered %d values with no dev-state file", len(devStates.current))
	}
	if err := devStates.writeDevState(); err != nil {
		t.Errorf("writeDevState: %v", err)
	}
}

// Stale or broken state must never stop the app from booting: the store keeps
// whatever it built itself.
func TestPreserveDevSurvivesBadState(t *testing.T) {
	restart := devSession(t)
	first := newCounterStore()
	PreserveDev("counters", first)
	first.add("x", 5)
	restart()

	second := newCounterStore()
	second.failOn = "restore"
	second.add("fresh", 1)
	PreserveDev("counters", second) // restore fails, reported, ignored
	if second.get("fresh") != 1 {
		t.Error("a failed restore clobbered the store's own state")
	}

	// A snapshot that fails is skipped, not fatal — and the other entries
	// still make it into the file.
	third := newCounterStore()
	third.failOn = "snapshot"
	PreserveDev("broken", third)
	ok := newCounterStore()
	PreserveDev("ok", ok)
	ok.add("y", 2)
	restart()

	revived := newCounterStore()
	PreserveDev("ok", revived)
	if revived.get("y") != 2 {
		t.Errorf("y = %d, want 2 (one bad snapshot must not lose the others)", revived.get("y"))
	}
}

func TestPreserveDevJSON(t *testing.T) {
	restart := devSession(t)

	state := map[string]int{"a": 1}
	PreserveDevJSON("m", func() map[string]int { return state }, func(v map[string]int) { state = v })
	state["b"] = 2

	restart()

	var restored map[string]int
	PreserveDevJSON("m", func() map[string]int { return restored }, func(v map[string]int) { restored = v })
	if restored["a"] != 1 || restored["b"] != 2 {
		t.Errorf("restored = %v, want {a:1 b:2}", restored)
	}
}

// The file is rewritten, not appended, and lands atomically — the next
// process starts reading it the instant this one exits.
func TestDevStateFileShape(t *testing.T) {
	restart := devSession(t)
	s := newCounterStore()
	PreserveDev("counters", s)
	s.add("x", 1)
	restart()

	path := os.Getenv(devStateEnv)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var f devStateFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if f.Version != 1 {
		t.Errorf("version = %d, want 1", f.Version)
	}
	if _, ok := f.Entries["counters"]; !ok {
		t.Errorf("entries = %v, want a counters key", f.Entries)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("the write-then-rename temp file was left behind")
	}
}

// A future format (or a corrupt file) is ignored rather than fed to stores.
func TestDevStateIgnoresUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":{"counters":"aGk="}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readDevStateFile(path); got != nil {
		t.Errorf("read a v99 state file: %v", got)
	}
	if got := readDevStateFile(filepath.Join(t.TempDir(), "absent.json")); got != nil {
		t.Errorf("read a missing file: %v", got)
	}
}
