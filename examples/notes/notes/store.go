package notes

import (
	"encoding/json"
	"sync"

	"github.com/paulmanoni/nexus"
)

// Note is the REST JSON body and the GraphQL type (driven by the struct tags).
type Note struct {
	ID    int    `json:"id" graphql:"id"`
	Title string `json:"title" graphql:"title,required" validate:"required"`
}

// Store is an in-memory notes store, provided into the DI graph by NewStore.
type Store struct {
	mu    sync.Mutex
	next  int
	notes map[int]Note
}

// NewStore is registered as a DI provider purely by its annotation — no
// hand-written nexus.Provide(NewStore) anywhere.
//
// PreserveDev keeps the notes you added by hand across a `nexus dev` rebuild,
// so a save doesn't send you back to an empty list. It does nothing outside
// nexus dev.
//
// @provide
func NewStore() *Store {
	s := &Store{next: 1, notes: map[int]Note{}}
	nexus.PreserveDev("notes", s)
	return s
}

// SnapshotDev / RestoreDev implement nexus.DevState.
func (s *Store) SnapshotDev() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.Marshal(storeState{Next: s.next, Notes: s.notes})
}

func (s *Store) RestoreDev(data []byte) error {
	var st storeState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next, s.notes = st.Next, st.Notes
	return nil
}

type storeState struct {
	Next  int          `json:"next"`
	Notes map[int]Note `json:"notes"`
}

func (s *Store) List() []Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Note, 0, len(s.notes))
	for id := 1; id < s.next; id++ {
		if n, ok := s.notes[id]; ok {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) Get(id int) (Note, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notes[id]
	return n, ok
}

func (s *Store) Create(title string) Note {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := Note{ID: s.next, Title: title}
	s.notes[s.next] = n
	s.next++
	return n
}
