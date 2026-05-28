package nexus

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestDevReloadRelevant(t *testing.T) {
	cases := []struct {
		name string
		ev   fsnotify.Event
		want bool
	}{
		// Source / served assets should reload.
		{"vue source", fsnotify.Event{Name: "islands.src/App.vue", Op: fsnotify.Write}, true},
		{"ts source", fsnotify.Event{Name: "main.ts", Op: fsnotify.Write}, true},
		{"css", fsnotify.Event{Name: "styles.css", Op: fsnotify.Create}, true},

		// Existing skips.
		{"sourcemap", fsnotify.Event{Name: "main.js.map", Op: fsnotify.Write}, false},
		{"hidden DS_Store", fsnotify.Event{Name: ".DS_Store", Op: fsnotify.Write}, false},
		{"chmod only", fsnotify.Event{Name: "App.vue", Op: fsnotify.Chmod}, false},

		// Runtime data artifacts must never reload — these are the
		// loop sources this filter exists to prevent.
		{"sqlite db", fsnotify.Event{Name: "data.db", Op: fsnotify.Write}, false},
		{"sqlite db create", fsnotify.Event{Name: "app.db", Op: fsnotify.Create}, false},
		{"sqlite ext", fsnotify.Event{Name: "data.sqlite", Op: fsnotify.Write}, false},
		{"sqlite3 ext", fsnotify.Event{Name: "data.sqlite3", Op: fsnotify.Write}, false},
		{"wal sidecar", fsnotify.Event{Name: "app.db-wal", Op: fsnotify.Write}, false},
		{"shm sidecar", fsnotify.Event{Name: "app.db-shm", Op: fsnotify.Write}, false},
		{"journal sidecar", fsnotify.Event{Name: "app.db-journal", Op: fsnotify.Write}, false},
		{"log file", fsnotify.Event{Name: "server.log", Op: fsnotify.Write}, false},
		{"uppercase DB", fsnotify.Event{Name: "DATA.DB", Op: fsnotify.Write}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := devReloadRelevant(tc.ev); got != tc.want {
				t.Fatalf("devReloadRelevant(%q, %v) = %v, want %v", tc.ev.Name, tc.ev.Op, got, tc.want)
			}
		})
	}
}

func TestValidDevReloadGlobs(t *testing.T) {
	got := validDevReloadGlobs([]string{"*.tmp", "", "  uploads/  ", "[bad", "cache/*.json"})
	want := []string{"*.tmp", "uploads", "cache/*.json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDevReloadExcluded(t *testing.T) {
	const root = "/proj"
	patterns := validDevReloadGlobs([]string{"*.tmp", "uploads", "cache/*.json"})
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"base glob", "/proj/sub/scratch.tmp", true},
		{"dir subtree shallow", "/proj/uploads/a.webm", true},
		{"dir subtree deep", "/proj/uploads/user1/take.webm", true},
		{"rel glob", "/proj/cache/index.json", true},
		{"rel glob wrong dir", "/proj/other/index.json", false},
		{"unmatched source", "/proj/islands.src/App.vue", false},
		{"dir name not subtree", "/proj/uploadsx/a.txt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := devReloadExcluded(tc.path, root, patterns); got != tc.want {
				t.Fatalf("devReloadExcluded(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
	if devReloadExcluded("/proj/anything.tmp", root, nil) {
		t.Fatal("nil patterns must never exclude")
	}
}
