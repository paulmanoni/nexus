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
	ID          string `json:"id" gorm:"primaryKey;size:36"`
	Name        string `json:"name" gorm:"size:200;not null"`
	Route       string `json:"route" gorm:"size:255;index"`
	Description string `json:"description" gorm:"type:text"`

	// Order is the play-order within a route. Lower = earlier.
	// When multiple tours share a route, the agent lists them
	// in this order and the "Preview all for this route" stitch
	// uses it too. Defaults to MAX(order)+1 on insert so new
	// tours land at the end.
	Order int `json:"order" gorm:"default:0"`

	// CoverImageURL is the FIRST clean screenshot of the page,
	// taken at recording-start. Drives the simple single-cover
	// preview path. Empty for legacy tours; preview falls back
	// to per-step screenshots when absent.
	CoverImageURL string `json:"cover_image_url,omitempty" gorm:"type:text"`

	// CoverImages is the full series of cover snapshots — one
	// at recording-start, plus one per Resume after Pause. Each
	// Step carries a CoverIndex pointing into this slice so the
	// preview can group steps by which page state was active
	// when they were captured. Items revealed inside dropdowns,
	// modals, or step-by-step flows render against the cover
	// that was active when they were clicked, instead of the
	// stale initial screenshot.
	//
	// Stored as JSON via gorm serializer; each entry is a base64
	// data URL the size of one viewport-sized PNG (~50-200 KB
	// each). For long tours that page through many states this
	// adds up — Phase 4 work moves these to a blob endpoint
	// with byte-range fetching.
	CoverImages []string `json:"cover_images,omitempty" gorm:"serializer:json;type:text"`

	// BaseWidth / BaseHeight are window.innerWidth and innerHeight
	// at recording-start. The preview locks the composite
	// wrapper's aspect-ratio to BaseWidth/BaseHeight and
	// positions each badge using rect_left/BaseWidth (horizontal)
	// and rect_top/BaseHeight (vertical). Storing both keeps
	// the math dimensionally correct — earlier versions used
	// BaseWidth for both axes which packed every badge into the
	// top-left corner of tall pages.
	BaseWidth  int `json:"base_width,omitempty"`
	BaseHeight int `json:"base_height,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

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

	// Rect (viewport-relative px at record time) drives the
	// composite preview layout: badges land at the step's
	// original screen position over the tour's cover image.
	// All four default to 0 when the step was recorded by a
	// pre-rect agent; the preview falls back to a stacked
	// per-step layout in that case.
	RectLeft   int `json:"rect_left,omitempty"`
	RectTop    int `json:"rect_top,omitempty"`
	RectWidth  int `json:"rect_width,omitempty"`
	RectHeight int `json:"rect_height,omitempty"`

	// MediaURL points at the screenshot/video for this step. May
	// be a relative URL the plugin serves (Phase 3 will add a
	// blob endpoint) or an external URL the operator pasted in.
	MediaURL string `json:"media_url" gorm:"size:1000"`

	// CoverIndex selects which Tour.CoverImages entry this step
	// was captured against. 0 = the initial screenshot (matches
	// tour.cover_image_url for backward compat). Bumps each
	// time the operator resumes from a pause; the preview
	// groups steps by this value so dropdown items render
	// against the cover that shows the dropdown OPEN, not the
	// stale recording-start state.
	CoverIndex int `json:"cover_index"`

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
