package pets

import (
	"context"
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/view"
)

// Board is a live page: its state lives on the server, one copy per
// connected page, and changes through events.
type Board struct {
	Pets    []Pet
	Adopted map[string]bool
}

func NewBoard() *Board { return &Board{} }

func (b *Board) Mount(ctx context.Context, store *Store) error {
	b.Pets = store.All()
	b.Adopted = map[string]bool{}
	return nil
}

// Adopt is an event: name comes from the browser, so it is checked.
func (b *Board) Adopt(ctx context.Context, name string) error {
	if !b.has(name) {
		return fmt.Errorf("no pet named %q", name)
	}
	b.Adopted[name] = true
	return nil
}

func (b *Board) Return(ctx context.Context, name string) error {
	delete(b.Adopted, name)
	return nil
}

func (b *Board) Clear(ctx context.Context) error {
	b.Adopted = map[string]bool{}
	return nil
}

func (b *Board) has(name string) bool {
	for _, p := range b.Pets {
		if p.Name == name {
			return true
		}
	}
	return false
}

// Module mounts the live board, declared like a nexus.Resource.
var Module = nexus.Module("pets",
	view.Live[*Board]("/board").
		Provide(NewBoard),
)
