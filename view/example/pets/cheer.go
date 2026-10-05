package pets

import (
	"context"

	"github.com/paulmanoni/nexus/v2/view"
)

// Cheer is a live component: each board keeps its own count of cheers, and
// a cheer renders only the component, not the board around it.
type Cheer struct {
	Pets   view.Assign[int]
	Cheers view.Assign[int]
}

// CheerProps is what the board passes it.
type CheerProps struct{ Pets int }

func (c *Cheer) Mount(ctx context.Context, p CheerProps) error {
	c.Pets.Set(p.Pets)
	return nil
}

// Update follows the board's pet count and keeps the cheers.
func (c *Cheer) Update(ctx context.Context, p CheerProps) error {
	c.Pets.Set(p.Pets)
	return nil
}

func (c *Cheer) Cheer(ctx context.Context) error {
	c.Cheers.Set(c.Cheers.Get() + 1)
	return nil
}
