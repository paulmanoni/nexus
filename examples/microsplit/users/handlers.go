package users

import (
	"github.com/paulmanoni/nexus"
)

// REST/GraphQL handler thin-wrappers. The framework's AsRest /
// AsQuery binders need handlers shaped (svc, Params[T]) → (R, error);
// these adapt the plain methods on Service to that shape. No business
// logic lives here.

func NewGet(svc *Service, p nexus.Params[GetArgs]) (*User, error) {
	return svc.Get(p.Context, p.Args)
}

func NewList(svc *Service, p nexus.Params[ListArgs]) ([]*User, error) {
	return svc.List(p.Context, p.Args)
}

func NewSearch(svc *Service, p nexus.Params[SearchArgs]) ([]*User, error) {
	return svc.Search(p.Context, p.Args)
}
