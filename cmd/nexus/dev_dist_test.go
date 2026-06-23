package main

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestDistSkipDir(t *testing.T) {
	cases := map[string]bool{
		"dist":         true,
		"node_modules": true,
		".git":         true,
		".vite":        true,
		"src":          false,
		"components":   false,
		"Pages":        false,
	}
	for name, want := range cases {
		if got := distSkipDir(name); got != want {
			t.Errorf("distSkipDir(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDistRelevant(t *testing.T) {
	cases := []struct {
		name string
		ev   fsnotify.Event
		want bool
	}{
		{"write src file", fsnotify.Event{Name: "/app/web/src/App.vue", Op: fsnotify.Write}, true},
		{"create file", fsnotify.Event{Name: "/app/web/src/new.ts", Op: fsnotify.Create}, true},
		{"rename file", fsnotify.Event{Name: "/app/web/src/old.ts", Op: fsnotify.Rename}, true},
		{"remove file", fsnotify.Event{Name: "/app/web/src/gone.ts", Op: fsnotify.Remove}, true},
		{"chmod only ignored", fsnotify.Event{Name: "/app/web/src/App.vue", Op: fsnotify.Chmod}, false},
		{"hidden file ignored", fsnotify.Event{Name: "/app/web/src/.DS_Store", Op: fsnotify.Write}, false},
	}
	for _, c := range cases {
		if got := distRelevant(c.ev); got != c.want {
			t.Errorf("%s: distRelevant = %v, want %v", c.name, got, c.want)
		}
	}
}
