// Package checkout demonstrates a module that *consumes* another
// module by importing its *Service directly. The same Go call —
// `svc.users.Get(ctx, users.GetArgs{ID: ...})` — works in monolith
// mode (direct method call) and in split mode (HTTP via the shadow
// generator's stub *users.Service), with no edits to this file when
// you peel users out into its own binary. The transport switch
// happens at compile time via `nexus build --deployment X`.
package checkout

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/microsplit/users"
)

// Service is checkout's service wrapper. Its constructor takes a
// *users.Service — the build tool decides at compile time whether
// that's the real local struct (monolith / users-svc binaries) or
// an HTTP-stub redefinition emitted by the shadow generator
// (checkout-svc binary). The type identifier is identical in both.
type Service struct {
	*nexus.Service
	users *users.Service
}

func NewService(app *nexus.App, u *users.Service) *Service {
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

// Module declares one POST endpoint. DeployAs makes checkout its own
// deployment unit so `nexus dev --split` boots it as a separate
// subprocess from users, exercising the real HTTP path between them
// via the codegen'd users.UsersClient.
var Module = nexus.Module("checkout",
	nexus.DeployAs("checkout-svc"),
	nexus.Provide(NewService),
	nexus.AsRest("POST", "/checkout", NewSubmit),
)
