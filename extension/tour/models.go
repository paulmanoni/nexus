package tour

import "time"

// Tour is the top-level walkthrough record. One Tour pins to one
// frontend Route pattern (exact match or glob — Phase 3 may add
// regex). At runtime the in-page agent looks up tours for the
// current route and renders them on demand.
//
// The Steps field is hydrated on read — flat rows in the store are
// rebuilt into the parent/child tree the runner walks. Writers
// pass the hydrated tree; the store flattens it back to rows on
// save.
type Tour struct {
	ID          string    `json:"id" gorm:"primaryKey;size:36"`
	Name        string    `json:"name" gorm:"size:200;not null"`
	Route       string    `json:"route" gorm:"size:255;index"`
	Description string    `json:"description" gorm:"type:text"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// Steps holds the hydrated tree on reads, and the new tree on
	// writes. NEVER stored as-is — the store flattens to rows.
	Steps []*Step `json:"steps" gorm:"-"`
}

// TableName overrides the gorm default ("tours") to make the
// plugin's tables namespace-prefixed. Avoids collisions with an
// app's own "tours" table.
func (Tour) TableName() string { return "nexus_tours" }

// Step is one stop on a walkthrough. ParentStepID is the self-FK
// that turns the flat table into a tree:
//
//   - NULL parent → root-level step (the major numbered milestones)
//   - non-NULL parent → substep (clarifies / drills into the parent)
//
// Order is 0-based within the parent (or within the tour for root
// steps). BadgeNumber is the human-visible "Step 3" badge baked
// onto the screenshot — independent of Order so operators can
// renumber for clarity without churning the underlying ordering.
type Step struct {
	ID           string  `json:"id" gorm:"primaryKey;size:36"`
	TourID       string  `json:"tour_id" gorm:"size:36;index;not null"`
	ParentStepID *string `json:"parent_step_id,omitempty" gorm:"size:36;index"`

	Order       int    `json:"order"`
	BadgeNumber int    `json:"badge_number"`
	Selector    string `json:"selector" gorm:"size:1000;not null"`
	Title       string `json:"title" gorm:"size:200"`
	Text        string `json:"text" gorm:"type:text"`
	Placement   string `json:"placement" gorm:"size:32;default:'bottom'"`
	Label       string `json:"label" gorm:"size:200"`

	// MediaURL points at the screenshot/video for this step. May
	// be a relative URL the plugin serves (Phase 3 will add a
	// blob endpoint) or an external URL the operator pasted in.
	MediaURL string `json:"media_url" gorm:"size:1000"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Children is hydrated on reads (recursive tree walk) and
	// consumed on writes (operator sends a nested payload).
	Children []*Step `json:"children,omitempty" gorm:"-"`
}

// TableName overrides the gorm default. Same prefix policy as
// Tour — namespaced so apps with their own "steps" table don't
// collide.
func (Step) TableName() string { return "nexus_tour_steps" }

// ReorderItem captures one row's new (parent, order) pair —
// batch-applied by Store.ReorderSteps. Operators reorder by
// drag-drop in the builder; the UI sends one ReorderSteps call
// with every affected row's new position rather than N
// individual updates.
type ReorderItem struct {
	StepID       string  `json:"step_id"`
	ParentStepID *string `json:"parent_step_id,omitempty"`
	Order        int     `json:"order"`
}
