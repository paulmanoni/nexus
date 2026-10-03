// Package pets holds the pet store and its components.
package pets

import (
	"strings"
	"sync"
)

type Pet struct{ Name, Kind string }

type Store struct {
	pets []Pet

	mu      sync.Mutex
	adopted map[string]bool // shared by every visitor
}

func NewStore() *Store {
	return &Store{adopted: map[string]bool{}, pets: []Pet{
		{"Biscuit", "dog"}, {"Mochi", "cat"}, {"Pepper", "dog"}, {"Kiwi", "parrot"},
		{"Nala", "cat"}, {"Bubbles", "fish"}, {"Rex", "tortoise"}, {"Luna", "rabbit"},
	}}
}

// All returns every pet.
func (s *Store) All() []Pet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Pet(nil), s.pets...)
}

// Add adds a pet.
func (s *Store) Add(p Pet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pets = append(s.pets, p)
}

func (s *Store) Search(q string) []Pet {
	s.mu.Lock()
	defer s.mu.Unlock()
	q = strings.ToLower(strings.TrimSpace(q))
	var out []Pet
	for _, p := range s.pets {
		if strings.Contains(strings.ToLower(p.Name), q) || strings.Contains(p.Kind, q) {
			out = append(out, p)
		}
	}
	return out
}

// Page returns one page of pets and the number of pages. page comes from
// the browser, so it is clamped.
func (s *Store) Page(page, size int) ([]Pet, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := (len(s.pets) + size - 1) / size
	page = min(max(page, 1), last)
	from := (page - 1) * size
	return s.pets[from:min(from+size, len(s.pets))], last
}

// Has reports whether a pet by that name exists.
func (s *Store) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pets {
		if p.Name == name {
			return true
		}
	}
	return false
}

// SetAdopted marks a pet adopted or not.
func (s *Store) SetAdopted(name string, adopted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if adopted {
		s.adopted[name] = true
	} else {
		delete(s.adopted, name)
	}
}

// ClearAdoptions returns every pet.
func (s *Store) ClearAdoptions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adopted = map[string]bool{}
}

// Remove takes a pet off the store.
func (s *Store) Remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.pets {
		if p.Name == name {
			s.pets = append(s.pets[:i:i], s.pets[i+1:]...)
			delete(s.adopted, name)
			return true
		}
	}
	return false
}

// Adopted is a copy of who is adopted.
func (s *Store) Adopted() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.adopted))
	for k, v := range s.adopted {
		out[k] = v
	}
	return out
}
