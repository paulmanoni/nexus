package tour

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the default Store — fully in-memory, sufficient
// for tests, demos, and dev runs of the plugin where persistence
// across restart doesn't matter. Production deployments wire
// WithGORM(*gorm.DB) instead.
//
// Concurrency: a single mutex guards both maps. The tour count is
// expected to be small (tens, not thousands) so contention isn't
// a concern; the simplicity wins.
type MemoryStore struct {
	mu    sync.RWMutex
	tours map[string]*Tour
	steps map[string]*Step // keyed by step ID
}

// NewMemoryStore returns an empty MemoryStore ready to use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		tours: map[string]*Tour{},
		steps: map[string]*Step{},
	}
}

// ListTours returns tours filtered by route (or all if route is
// empty). Steps are NOT hydrated — list views show metadata
// only; callers fetch individual tours for the tree.
func (m *MemoryStore) ListTours(_ context.Context, route string) ([]*Tour, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tour, 0, len(m.tours))
	for _, t := range m.tours {
		if route != "" && t.Route != route {
			continue
		}
		// Shallow copy to keep the caller from mutating store state.
		cp := *t
		cp.Steps = nil
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetTour returns one tour with its full step tree hydrated.
func (m *MemoryStore) GetTour(_ context.Context, id string) (*Tour, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tours[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *t
	// Collect all steps for this tour, then build tree. We have
	// to clone because the caller may mutate.
	flat := make([]*Step, 0)
	for _, s := range m.steps {
		if s.TourID != id {
			continue
		}
		sc := *s
		flat = append(flat, &sc)
	}
	cp.Steps = buildTree(flat)
	return &cp, nil
}

// UpsertTour writes the tour and replaces its full step tree.
// Existing steps not in the new tree are dropped — the editor
// view is authoritative.
func (m *MemoryStore) UpsertTour(_ context.Context, t *Tour) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	existing, found := m.tours[t.ID]
	if found {
		t.CreatedAt = existing.CreatedAt
	} else {
		if t.CreatedAt.IsZero() {
			t.CreatedAt = now
		}
	}
	t.UpdatedAt = now

	// Wipe existing steps for this tour, then re-flatten the new
	// tree and store each row keyed by id.
	for id, s := range m.steps {
		if s.TourID == t.ID {
			delete(m.steps, id)
		}
	}
	flat := flattenTree(t.ID, t.Steps)
	for _, s := range flat {
		s.CreatedAt = now
		s.UpdatedAt = now
		// Clone to avoid sharing pointers with the caller.
		cp := *s
		cp.Children = nil
		m.steps[cp.ID] = &cp
	}

	// Store the tour itself without the hydrated tree.
	tcp := *t
	tcp.Steps = nil
	m.tours[t.ID] = &tcp
	return nil
}

// DeleteTour removes a tour and every step belonging to it.
func (m *MemoryStore) DeleteTour(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tours[id]; !ok {
		return ErrNotFound
	}
	delete(m.tours, id)
	for sid, s := range m.steps {
		if s.TourID == id {
			delete(m.steps, sid)
		}
	}
	return nil
}

// UpsertStep writes one step (used by the in-page recorder for
// incremental "one click = one step" appends).
func (m *MemoryStore) UpsertStep(_ context.Context, s *Step) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tours[s.TourID]; !ok {
		return ErrNotFound
	}
	now := time.Now().UTC()
	if existing, ok := m.steps[s.ID]; ok {
		s.CreatedAt = existing.CreatedAt
	} else if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	cp := *s
	cp.Children = nil
	m.steps[s.ID] = &cp
	return nil
}

// DeleteStep removes one step. Children reparent to the deleted
// step's parent so substep work isn't accidentally orphaned.
func (m *MemoryStore) DeleteStep(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	victim, ok := m.steps[id]
	if !ok {
		return ErrNotFound
	}
	// Reparent children of the victim.
	for _, s := range m.steps {
		if s.ParentStepID != nil && *s.ParentStepID == id {
			s.ParentStepID = victim.ParentStepID
		}
	}
	delete(m.steps, id)
	return nil
}

// ReorderSteps applies a batch of (parent, order) updates.
// Atomic — validates every ID exists before mutating anything.
func (m *MemoryStore) ReorderSteps(_ context.Context, tourID string, items []ReorderItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Validate first — atomicity means "all or nothing."
	for _, it := range items {
		s, ok := m.steps[it.StepID]
		if !ok || s.TourID != tourID {
			return ErrNotFound
		}
	}
	now := time.Now().UTC()
	for _, it := range items {
		s := m.steps[it.StepID]
		s.ParentStepID = it.ParentStepID
		s.Order = it.Order
		s.UpdatedAt = now
	}
	return nil
}