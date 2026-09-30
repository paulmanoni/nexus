package pets

import (
	"context"
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/view"
)

// Board is a live page: its state lives on the server, one copy per
// connected page. Adoptions are shared: every event broadcasts, and every
// open board refreshes.
type Board struct {
	Pets    []Pet
	Adopted map[string]bool
}

func NewBoard() *Board { return &Board{} }

// adoptions is the topic every board listens on.
const adoptions = "adoptions"

func (b *Board) Mount(ctx context.Context, sock *view.Socket, store *Store) error {
	sock.Subscribe(adoptions)
	b.Pets = store.All()
	b.Adopted = store.Adopted()
	return nil
}

// Adopt is an event: name comes from the browser, so it is checked.
func (b *Board) Adopt(ctx context.Context, store *Store, name string) error {
	if !store.Has(name) {
		return fmt.Errorf("no pet named %q", name)
	}
	store.SetAdopted(name, true)
	view.Broadcast(ctx, adoptions, name)
	return nil
}

func (b *Board) Return(ctx context.Context, store *Store, name string) error {
	store.SetAdopted(name, false)
	view.Broadcast(ctx, adoptions, name)
	return nil
}

func (b *Board) Clear(ctx context.Context, store *Store) error {
	store.ClearAdoptions()
	view.Broadcast(ctx, adoptions, "")
	return nil
}

// Info runs on every board when anyone's adoption changes.
func (b *Board) Info(ctx context.Context, store *Store, msg view.Message) error {
	b.Adopted = store.Adopted()
	return nil
}

// Module mounts the live board, declared like a nexus.Resource.
var Module = nexus.Module("pets",
	view.Live[*Board]("/board").
		Provide(NewBoard),
)
