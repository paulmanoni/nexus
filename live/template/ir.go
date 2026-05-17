package template

// IR (intermediate representation) types produced by Lower(*Template).
// A Fragment is a unit of "renderable template" — at runtime the
// interpreter walks one against component state to produce a
// Rendered. Codegen, if added later, would emit Go source from these
// same types.
//
// The shape mirrors Rendered's static/dynamic split, deliberately:
//
//	Fragment.Statics  ←—— Rendered.S
//	Fragment.Slots    ←—— Rendered.D (after evaluation)
//
// Invariant: len(Statics) = len(Slots) + 1. An "empty" Fragment is
// {Statics: [""], Slots: []}. This keeps Render-side glue uniform
// (S[0] + D[0] + S[1] + ... + S[n]).

// Fragment is a runnable template chunk: literal HTML interleaved
// with Slot values that the interpreter evaluates against state.
type Fragment struct {
	Statics []string
	Slots   []Slot
}

// Slot is one dynamic insertion point in a Fragment. Each concrete
// kind tells the interpreter how to produce the value for that
// position in the rendered output.
type Slot interface {
	isSlot()
	SlotPos() Position
}

// ExprSlot evaluates a Go-like expression and inserts its string
// rendering (HTML-escaped at interpret time). Covers both
// {{ interp }} text sites and :attr="x" / nl-bind:attr="x"
// attribute bindings — the surrounding static carries the attr= and
// closing-quote bytes.
type ExprSlot struct {
	Expr string
	Pos  Position
}

func (ExprSlot) isSlot()             {}
func (s ExprSlot) SlotPos() Position { return s.Pos }

// IslandPropsSlot evaluates one expression, JSON-encodes the
// resulting value, and emits an HTML-escaped string suitable for
// embedding in a data-style attribute. The client parses it back
// with JSON.parse(el.getAttribute("nl-island-props")) and hands
// the result to the island's mount(el, props, channel). JSON
// preserves type info (ints vs strings vs nested objects) that
// would be lossy through a plain string interpolation.
//
// Static, when true, marks the slot as evaluate-once-per-session:
// the first render computes + caches the value, subsequent
// re-renders return the cached value untouched. That makes the
// island's :props value invisible to the diff stream after
// initial paint — useful for big snapshots where updates flow
// via ctx.PushIsland rather than auto-flowing through diffs.
// Triggered by the `static-props` attribute on <nl-island/>.
type IslandPropsSlot struct {
	Expr   string
	Static bool
	Pos    Position
}

func (IslandPropsSlot) isSlot()             {}
func (s IslandPropsSlot) SlotPos() Position { return s.Pos }

// ArgsSlot evaluates each expression in Exprs, JSON-encodes the
// values, and emits a JSON array literal — the wire form the
// client reads from data-nl-args to populate payload.args for
// call-form event handlers (@click="like(Post.ID, 'note')").
//
// JSON instead of HTML-escaped string concatenation so type info
// survives the round-trip: an int stays an int, a bool stays a
// bool, the server dispatch can convert each value to the
// handler's typed parameter without guessing.
type ArgsSlot struct {
	Exprs []string
	Pos   Position
}

func (ArgsSlot) isSlot()             {}
func (s ArgsSlot) SlotPos() Position { return s.Pos }

// BranchSlot is an nl-if / nl-else-if / nl-else chain. Branches are
// evaluated in order; the first whose Cond is true (or whose Cond is
// "" — the else terminator) wins. If none match, the slot renders as
// nil (no HTML), which the diff layer treats as an empty insertion.
type BranchSlot struct {
	Branches []Branch
	Pos      Position
}

func (BranchSlot) isSlot()           {}
func (s BranchSlot) SlotPos() Position { return s.Pos }

// Branch is one arm of a BranchSlot. Cond == "" marks the else arm
// (always the last branch when present); for nl-if and nl-else-if,
// Cond holds the original expression text.
type Branch struct {
	Cond     string
	Fragment *Fragment
}

// LoopSlot is an nl-for binding. Var is the iteration variable name
// bound inside Body; Iter is the expression yielding the iterable;
// KeyExpr is the :key="..." expression used by the diff algorithm
// to identify rows across renders (required — the parser will reject
// nl-for without :key in a later pass; for v1 the lowering accepts
// missing :key and KeyExpr will be empty).
type LoopSlot struct {
	Var     string
	Iter    string
	KeyExpr string
	Body    *Fragment
	Pos     Position
}

func (LoopSlot) isSlot()           {}
func (s LoopSlot) SlotPos() Position { return s.Pos }

// ComponentSlot is a reference to another component (<UserCard ...>).
// At interpret time, the engine resolves Name to a registered
// Component, instantiates it with Props, and renders it in-place.
// Children (slot bodies) are recorded but not yet routed — slot
// dispatch is a v2 concern.
type ComponentSlot struct {
	Name     string
	Props    []ComponentProp
	Children *Fragment // default-slot body; nil when component has no children
	Pos      Position
}

func (ComponentSlot) isSlot()           {}
func (s ComponentSlot) SlotPos() Position { return s.Pos }

// ComponentProp is one prop passed to a child component. IsBind
// distinguishes a literal value (":compact=\"true\"" doesn't get
// IsBind=true at lowering — it stays as a literal Value) from a
// dynamic expression (:user="u" → IsBind=true, Value="u").
type ComponentProp struct {
	Name   string
	Value  string
	IsBind bool
	Pos    Position
}
