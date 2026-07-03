package notes

import (
	"fmt"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/examples/notes/widgets"
)

type listArgs struct{}

type getArgs struct {
	ID int `uri:"id"` // path param `:id` binds via the `uri` tag
}

type createArgs struct {
	Title string `json:"title" graphql:"title,required" validate:"required"`
}

// NewListNotes → REST GET /notes. svc grounds it under the notes service;
// *Store is DI-injected (provided by the annotated NewStore).
//
// @rest GET /notes
func NewListNotes(svc *Service, store *Store, p nexus.Params[listArgs]) ([]Note, error) {
	return store.List(), nil
}

// @rest GET /notes/:id
func NewGetNote(svc *Service, store *Store, p nexus.Params[getArgs]) (*Note, error) {
	n, ok := store.Get(p.Args.ID)
	if !ok {
		return nil, fmt.Errorf("note %d not found", p.Args.ID)
	}
	return &n, nil
}

// @rest POST /notes
func NewCreateNote(svc *Service, store *Store, p nexus.Params[createArgs]) (*Note, error) {
	n := store.Create(p.Args.Title)
	return &n, nil
}

// NewSearchNotes → GraphQL query `searchNotes`. Same handler shape, different
// transport — one annotation switches it.
//
// @query
func NewSearchNotes(svc *Service, store *Store, p nexus.Params[listArgs]) ([]Note, error) {
	return store.List(), nil
}

type healthResp struct {
	OK bool `json:"ok"`
}

// NewStatsPanel uses a CUSTOM extension decorator (//@widgets.Panel) rather
// than a built-in keyword — the codegen emits widgets.Panel("/stats",
// NewStatsPanel), resolving the widgets import from this file (it's imported for
// the *widgets.Stats return type). Mounts at GET /widgets/stats.
//
// @widgets.Panel "/stats"
func NewStatsPanel(svc *Service, store *Store, p nexus.Params[listArgs]) (*widgets.Stats, error) {
	return &widgets.Stats{Count: len(store.List())}, nil
}

// @rest GET /health
func NewHealth(svc *Service, p nexus.Params[listArgs]) (*healthResp, error) {
	return &healthResp{OK: true}, nil
}
