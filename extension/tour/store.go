package tour

import (
	"context"
	"errors"
)

// Store is the persistence seam. Two implementations ship:
// MemoryStore (default — for dev / tests) and GormStore
// (production — backed by *gorm.DB). Operators pick via the
// WithGORM option; the in-memory store is the silent default.
//
// All methods take a context; concrete stores plumb it through
// to gorm.WithContext so timeouts + tracing flow correctly.
type Store interface {
	// ListTours returns every tour. When route is non-empty,
	// filters to tours pinned to that route. Steps are not
	// hydrated — list views show the tour metadata only;
	// callers fetch individual tours via GetTour for the
	// full tree.
	ListTours(ctx context.Context, route string) ([]*Tour, error)

	// GetTour returns one tour with its full step tree
	// hydrated (children recursively populated).
	GetTour(ctx context.Context, id string) (*Tour, error)

	// UpsertTour creates or updates a tour AND its full step
	// tree atomically. Existing steps not present in t.Steps
	// are deleted (the operator's editor view is authoritative).
	UpsertTour(ctx context.Context, t *Tour) error

	// DeleteTour removes a tour and cascades to its steps.
	DeleteTour(ctx context.Context, id string) error

	// UpsertStep creates or updates a single step. Used for
	// incremental edits from the in-page recorder (one click =
	// one new step) where rewriting the whole tour every time
	// would be wasteful.
	UpsertStep(ctx context.Context, s *Step) error

	// DeleteStep removes one step. Children are reparented to
	// the deleted step's parent (so an accidental delete of an
	// intermediate node doesn't orphan the substeps below it).
	DeleteStep(ctx context.Context, id string) error

	// ReorderSteps applies a batch of (parent, order) updates
	// for one tour. Atomic — either every row updates or none.
	ReorderSteps(ctx context.Context, tourID string, items []ReorderItem) error

	// ReorderTours rewrites the Order field on a batch of tours.
	// Caller supplies ids in the new ordering; the store assigns
	// 0..n-1 along that sequence. Atomic.
	ReorderTours(ctx context.Context, ids []string) error
}

// ErrNotFound is returned by Store methods when the requested
// id doesn't exist. Handlers translate this into HTTP 404.
var ErrNotFound = errors.New("tour: not found")
