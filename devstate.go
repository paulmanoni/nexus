package nexus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Dev-state preservation: carrying in-memory state across a `nexus dev`
// rebuild.
//
// A rebuild replaces the process, and Go has no code hot-swap, so anything
// living in a map dies with the old binary — the seeded users, the notes you
// just POSTed, the fixtures you set up by hand. The alternative to losing them
// is to hand the state to the dev loop on the way out and take it back on the
// way in, which is what this does:
//
//	func NewStore() *Store {
//	    s := &Store{notes: map[int]Note{}}
//	    nexus.PreserveDev("notes", s)   // no-op outside `nexus dev`
//	    return s
//	}
//
//	func (s *Store) SnapshotDev() ([]byte, error) { return json.Marshal(s.notes) }
//	func (s *Store) RestoreDev(b []byte) error    { return json.Unmarshal(b, &s.notes) }
//
// Restore happens inside PreserveDev, so it doesn't matter when the DI
// container gets around to constructing the value. The snapshot is written on
// graceful shutdown, which is exactly what `nexus dev` triggers before it
// swaps in the new binary.
//
// Scope and limits, on purpose:
//
//   - Dev only. Outside `nexus dev` (no NEXUS_DEV_STATE in the environment)
//     PreserveDev registers nothing and no file is ever written, so a
//     production binary carries a no-op.
//   - Per session. The state file lives in the dev session's temp dir and dies
//     with it: state survives rebuilds, not a Ctrl-C.
//   - Graceful exits only. A SIGKILL or a panic skips the snapshot; the app
//     comes back empty, as it would have anyway.
//   - Best-effort. A snapshot or restore that fails is reported and skipped —
//     stale state from a struct you just reshaped must never stop the app from
//     booting.

// devStateEnv names the file `nexus dev` gives the child for its state. Unset
// everywhere else, which is what makes this dev-only.
const devStateEnv = "NEXUS_DEV_STATE"

// DevState is implemented by values that can hand their in-memory contents to
// the dev loop and take them back after a rebuild. Both halves speak opaque
// bytes, so the value picks its own encoding (JSON, gob, protobuf, …).
type DevState interface {
	SnapshotDev() ([]byte, error)
	RestoreDev(data []byte) error
}

// PreserveDev registers v under name so `nexus dev` carries its state across
// rebuilds, and immediately restores the state a previous build left behind.
//
// Call it wherever the value is created — a constructor is the natural place.
// Outside `nexus dev` it does nothing at all. Names are per-app identifiers;
// registering the same name twice replaces the earlier value (the later one
// wins, which is what a re-created singleton wants).
func PreserveDev(name string, v DevState) {
	devStates.preserve(name, v)
}

// PreserveDevJSON is the zero-ceremony form for state you can marshal
// directly, without writing the DevState methods:
//
//	nexus.PreserveDevJSON("counters",
//	    func() map[string]int { return s.snapshot() },
//	    func(m map[string]int) { s.load(m) })
//
// get is called on shutdown, set on startup when a snapshot exists. Both must
// be safe to call from another goroutine — take the value's own lock inside
// them, as the store's regular methods do.
func PreserveDevJSON[T any](name string, get func() T, set func(T)) {
	PreserveDev(name, jsonDevState[T]{get: get, set: set})
}

type jsonDevState[T any] struct {
	get func() T
	set func(T)
}

func (j jsonDevState[T]) SnapshotDev() ([]byte, error) { return json.Marshal(j.get()) }

func (j jsonDevState[T]) RestoreDev(b []byte) error {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	j.set(v)
	return nil
}

// devStateRegistry holds what the running app agreed to preserve, plus the
// blobs the previous build left behind.
type devStateRegistry struct {
	mu       sync.Mutex
	path     string
	loaded   bool
	previous map[string][]byte
	current  map[string]DevState
	order    []string
}

var devStates = &devStateRegistry{}

func (r *devStateRegistry) preserve(name string, v DevState) {
	if v == nil {
		return
	}
	path := os.Getenv(devStateEnv)
	if path == "" {
		return // not under `nexus dev`
	}
	r.mu.Lock()
	r.path = path
	if !r.loaded {
		r.previous = readDevStateFile(path)
		r.loaded = true
		r.current = map[string]DevState{}
	}
	if _, dup := r.current[name]; !dup {
		r.order = append(r.order, name)
	}
	r.current[name] = v
	blob, ok := r.previous[name]
	r.mu.Unlock()

	if !ok {
		return
	}
	// Restore outside the lock: RestoreDev takes the value's own lock, and a
	// store whose restore path touches other preserved values would otherwise
	// deadlock against us.
	if err := v.RestoreDev(blob); err != nil {
		fmt.Fprintf(os.Stderr, "nexus: dev-state %q not restored: %v\n", name, err)
	}
}

// writeDevState snapshots every registered value into the session's state
// file. Called from the shutdown hook Run installs in dev.
func (r *devStateRegistry) writeDevState() error {
	r.mu.Lock()
	path := r.path
	names := append([]string(nil), r.order...)
	values := make(map[string]DevState, len(r.current))
	for k, v := range r.current {
		values[k] = v
	}
	r.mu.Unlock()

	if path == "" || len(names) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(names))
	sort.Strings(names)
	for _, name := range names {
		blob, err := values[name].SnapshotDev()
		if err != nil {
			fmt.Fprintf(os.Stderr, "nexus: dev-state %q not saved: %v\n", name, err)
			continue
		}
		out[name] = blob
	}
	data, err := json.Marshal(devStateFile{Version: 1, Entries: out})
	if err != nil {
		return err
	}
	// Write-then-rename: the dev loop starts the next process the moment this
	// one exits, and it must never read a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// devStateFile is the on-disk shape. []byte marshals as base64, so a value's
// encoding stays its own business.
type devStateFile struct {
	Version int               `json:"version"`
	Entries map[string][]byte `json:"entries"`
}

func readDevStateFile(path string) map[string][]byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f devStateFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 1 {
		return nil
	}
	return f.Entries
}

// DevStateDir returns the directory `nexus dev` set aside for state that
// should outlive a rebuild, or "" when the process isn't running under
// `nexus dev`.
//
// PreserveDev is the right tool for state you can hand over as bytes. This
// is the escape hatch for state that already has its own on-disk format —
// an embedded key/value store, a SQLite file — where the whole fix for
// "my session dies on every save" is pointing it at a real path instead
// of ":memory:". Same lifetime either way: the directory is per dev
// session, so what you write survives rebuilds but not a Ctrl-C.
func DevStateDir() string {
	if os.Getenv(devStateEnv) == "" {
		return ""
	}
	return devStateDir()
}

// devStateDir is where the CLI puts the file; exposed for tests and for the
// error message when the directory is gone.
func devStateDir() string { return filepath.Dir(os.Getenv(devStateEnv)) }
