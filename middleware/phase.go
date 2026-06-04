package middleware

// Phase orders middleware uniformly across transports (see
// docs/design/middleware-redesign.md §7). Lower runs further out. Built-ins
// pick a phase; nexus.Use defaults to PhaseApp.
//
// Declared now so step 3's chain builder can sort by it; nothing reads Phase
// in steps 1–2.
type Phase int

const (
	PhaseRecover Phase = iota // outermost: panic boundary
	PhaseObserve              // trace, metrics, request-id
	PhaseCORS                 // skipped by the WS frame chain
	PhaseRateLimit
	PhaseAuth  // identity resolution
	PhaseAuthz // Requires(...) gates
	PhaseApp   // user nexus.Use(...), default
)
