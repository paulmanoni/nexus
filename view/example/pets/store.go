// Package pets holds the pet store and its components.
package pets

import "strings"

type Pet struct{ Name, Kind string }

type Store struct{ pets []Pet }

func NewStore() *Store {
	return &Store{pets: []Pet{
		{"Biscuit", "dog"}, {"Mochi", "cat"}, {"Pepper", "dog"}, {"Kiwi", "parrot"},
		{"Nala", "cat"}, {"Bubbles", "fish"}, {"Rex", "tortoise"}, {"Luna", "rabbit"},
	}}
}

// All returns every pet.
func (s *Store) All() []Pet { return append([]Pet(nil), s.pets...) }

func (s *Store) Search(q string) []Pet {
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
	last := (len(s.pets) + size - 1) / size
	page = min(max(page, 1), last)
	from := (page - 1) * size
	return s.pets[from:min(from+size, len(s.pets))], last
}
