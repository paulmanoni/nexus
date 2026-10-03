package pets

import (
	"context"
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/view"
)

// Board is a live page: its state lives on the server, one copy per
// connected page. Adoptions are shared: every event broadcasts, and every
// open board refreshes.
type Board struct {
	Pets    []Pet
	Adopted map[string]bool
	Draft   PetInput // the add form, as last validated
}

// PetInput is the add form: bound from its fields by their form tags.
type PetInput struct {
	Name string `form:"name"`
	Kind string `form:"kind"`
}

func (in PetInput) check(store *Store) error {
	errs := nexus.NewErrors()
	switch name := strings.TrimSpace(in.Name); {
	case name == "":
		errs.Field("name", "a name is required")
	case store.Has(name):
		errs.Field("name", name+" is already here")
	}
	if strings.TrimSpace(in.Kind) == "" {
		errs.Field("kind", "what kind of pet?")
	}
	if errs.Any() {
		return errs
	}
	return nil
}

// Validate runs as the add form is typed into.
func (b *Board) Validate(ctx context.Context, store *Store, in PetInput) error {
	b.Draft = in
	return in.check(store)
}

// Add is the add form's submit: every open board gets the new pet.
func (b *Board) Add(ctx context.Context, store *Store, in PetInput) error {
	b.Draft = in
	if err := in.check(store); err != nil {
		return err
	}
	store.Add(Pet{Name: strings.TrimSpace(in.Name), Kind: strings.TrimSpace(in.Kind)})
	b.Draft = PetInput{}
	view.Broadcast(ctx, adoptions, in.Name)
	return nil
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

// Info runs on every board when anyone's adoption or a new pet arrives.
func (b *Board) Info(ctx context.Context, store *Store, msg view.Message) error {
	b.Pets = store.All()
	b.Adopted = store.Adopted()
	return nil
}

// Module mounts the live board, declared like a nexus.Resource.
var Module = nexus.Module("pets",
	view.Live[*Board]("/board").
		Provide(NewBoard),
)
