package tour

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// GormStore is the production Store implementation. Backed by a
// *gorm.DB the operator supplies via WithGORM(db). On first use
// the store auto-migrates its two tables (nexus_tours,
// nexus_tour_steps) — safe to call against an existing schema
// (gorm AutoMigrate is idempotent and additive-only).
type GormStore struct {
	db *gorm.DB
}

// NewGormStore wires a fresh store + runs AutoMigrate. The error
// surfaces missing-permission / locked-DB situations early so the
// plugin doesn't half-boot.
func NewGormStore(db *gorm.DB) (*GormStore, error) {
	if db == nil {
		return nil, errors.New("tour: NewGormStore requires a non-nil *gorm.DB")
	}
	s := &GormStore{db: db}
	if err := db.AutoMigrate(&Tour{}, &Step{}); err != nil {
		return nil, fmt.Errorf("tour: AutoMigrate: %w", err)
	}
	return s, nil
}

// ListTours filters by route when set; otherwise returns all
// tours sorted by name. Steps are intentionally not joined —
// callers fetch the tree via GetTour.
func (g *GormStore) ListTours(ctx context.Context, route string) ([]*Tour, error) {
	var rows []*Tour
	q := g.db.WithContext(ctx).Order("name ASC")
	if route != "" {
		q = q.Where("route = ?", route)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("tour: ListTours: %w", err)
	}
	return rows, nil
}

// GetTour pulls the tour row + every step row in one query each,
// then runs buildTree to hydrate. Two round trips beats a recursive
// CTE for the tour-sized data we expect (low dozens of steps).
func (g *GormStore) GetTour(ctx context.Context, id string) (*Tour, error) {
	var t Tour
	if err := g.db.WithContext(ctx).Where("id = ?", id).First(&t).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tour: GetTour: %w", err)
	}
	var flat []*Step
	if err := g.db.WithContext(ctx).
		Where("tour_id = ?", id).
		Order("\"order\" ASC").
		Find(&flat).Error; err != nil {
		return nil, fmt.Errorf("tour: GetTour steps: %w", err)
	}
	t.Steps = buildTree(flat)
	return &t, nil
}

// UpsertTour writes the tour row + replaces its step tree in a
// single transaction so a partial save can't leave orphaned
// rows in the steps table.
func (g *GormStore) UpsertTour(ctx context.Context, t *Tour) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Save or update the tour row. gorm Save handles both
		// insert (no existing PK) and update (existing PK).
		if err := tx.Save(t).Error; err != nil {
			return fmt.Errorf("save tour: %w", err)
		}

		// Wipe existing steps for this tour, then re-insert from
		// the flattened tree. Cheaper than diff-update for the
		// size we expect; the editor view is authoritative.
		if err := tx.Where("tour_id = ?", t.ID).Delete(&Step{}).Error; err != nil {
			return fmt.Errorf("clear steps: %w", err)
		}
		flat := flattenTree(t.ID, t.Steps)
		if len(flat) == 0 {
			return nil
		}
		// CreateInBatches keeps the insert sane on larger trees
		// without blowing past Postgres's 65k-parameter ceiling.
		if err := tx.CreateInBatches(flat, 100).Error; err != nil {
			return fmt.Errorf("insert steps: %w", err)
		}
		return nil
	})
}

// DeleteTour removes the tour row and every step belonging to it.
// Transaction so a half-delete can't leave dangling rows.
func (g *GormStore) DeleteTour(ctx context.Context, id string) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ?", id).Delete(&Tour{})
		if res.Error != nil {
			return fmt.Errorf("delete tour: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		if err := tx.Where("tour_id = ?", id).Delete(&Step{}).Error; err != nil {
			return fmt.Errorf("delete tour steps: %w", err)
		}
		return nil
	})
}

// UpsertStep writes one step row. Used by the in-page recorder
// for incremental appends. Validates the parent tour exists so
// orphan steps can't be created via this path.
func (g *GormStore) UpsertStep(ctx context.Context, s *Step) error {
	// Existence check is intentional — a successful save against
	// a missing tour_id would silently orphan the step. ErrNotFound
	// surfaces faster than an FK constraint violation message.
	var count int64
	if err := g.db.WithContext(ctx).Model(&Tour{}).
		Where("id = ?", s.TourID).Count(&count).Error; err != nil {
		return fmt.Errorf("check tour: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	if err := g.db.WithContext(ctx).Save(s).Error; err != nil {
		return fmt.Errorf("save step: %w", err)
	}
	return nil
}

// DeleteStep removes one step and reparents its children to the
// deleted step's parent. The reparent is one UPDATE — the
// flat-with-self-FK schema makes this cheap.
func (g *GormStore) DeleteStep(ctx context.Context, id string) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var victim Step
		if err := tx.Where("id = ?", id).First(&victim).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lookup step: %w", err)
		}
		// Reparent the victim's children. A NULL ParentStepID
		// promotes them to roots; a non-NULL one slots them
		// alongside the victim's siblings.
		if err := tx.Model(&Step{}).
			Where("parent_step_id = ?", id).
			Update("parent_step_id", victim.ParentStepID).Error; err != nil {
			return fmt.Errorf("reparent children: %w", err)
		}
		if err := tx.Where("id = ?", id).Delete(&Step{}).Error; err != nil {
			return fmt.Errorf("delete step: %w", err)
		}
		return nil
	})
}

// ReorderSteps applies the batch of (parent, order) edits the
// editor's drag-drop UI emits. Atomic: every row updates or
// none (transaction-wrapped).
func (g *GormStore) ReorderSteps(ctx context.Context, tourID string, items []ReorderItem) error {
	if len(items) == 0 {
		return nil
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Validate first — a single missing/mismatched step
		// fails the whole batch.
		ids := make([]string, len(items))
		for i, it := range items {
			ids[i] = it.StepID
		}
		var count int64
		if err := tx.Model(&Step{}).
			Where("id IN ? AND tour_id = ?", ids, tourID).
			Count(&count).Error; err != nil {
			return fmt.Errorf("validate reorder: %w", err)
		}
		if int(count) != len(items) {
			return ErrNotFound
		}
		for _, it := range items {
			if err := tx.Model(&Step{}).Where("id = ?", it.StepID).
				Updates(map[string]any{
					"parent_step_id": it.ParentStepID,
					"order":          it.Order,
				}).Error; err != nil {
				return fmt.Errorf("apply reorder: %w", err)
			}
		}
		return nil
	})
}