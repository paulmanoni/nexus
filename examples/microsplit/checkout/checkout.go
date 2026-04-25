// Package checkout demonstrates a module that *consumes* another
// module via its generated typed client. The same Go call —
// `users.Get(ctx, users.GetArgs{ID: ...})` — works in monolith mode
// (in-process LocalInvoker) and in split mode (HTTP RemoteCaller),
// with no edits to checkout's handler code when you decide to peel
// users out into its own binary.
package checkout

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

// Service is checkout's service wrapper. Its constructor takes a
// users.UsersClient — the framework injects either the local or
// remote variant depending on the running binary's deployment, but
// the type the consumer sees is identical.
type Service struct {
	*nexus.Service
	users users.UsersClient
}

func NewService(app *nexus.App, u users.UsersClient) *Service {
	return &Service{
		Service: app.Service("checkout").Describe("Order checkout"),
		users:   u,
	}
}

type Receipt struct {
	OrderID string `json:"orderId"`
	UserID  string `json:"userId"`
	Display string `json:"display"`
}

type SubmitArgs struct {
	UserID  string `json:"userId"`
	OrderID string `json:"orderId"`
}

// NewSubmit fetches the user via the typed client (monolith: direct
// invoke; split: HTTP) and returns a receipt that includes the user's
// display name. The handler is unaware of which transport ran the
// users.Get call — this is the framework's contract.
func NewSubmit(svc *Service, p nexus.Params[SubmitArgs]) (*Receipt, error) {
	u, err := svc.users.Get(p.Context, users.GetArgs{ID: p.Args.UserID})
	if err != nil {
		return nil, fmt.Errorf("lookup user: %w", err)
	}
	return &Receipt{
		OrderID: p.Args.OrderID,
		UserID:  u.ID,
		Display: u.Name,
	}, nil
}

// Module declares one POST endpoint. checkout has no DeployAs tag of
// its own in the demo — the split here is "users out, checkout local".
// Tag it the same way to make it splittable on its own.
var Module = nexus.Module("checkout",
	nexus.Provide(NewService),
	nexus.Provide(users.NewUsersClient),
	nexus.AsRest("POST", "/checkout", NewSubmit),
)