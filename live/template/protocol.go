package template

// Wire protocol message envelopes. JSON-marshaled. Field tags follow
// the LiveView convention of short keys ("d" for diff, "r" for
// rendered) to keep diffs compact on the wire — most frames in a
// session are diffs, and we ship one per event.

// Inbound is the discriminated union of all client → server messages.
// Dispatch is by Type; remaining fields are populated per-Type:
//
//	"join":  Component, Params       (one-shot at session start)
//	"event": Name, Payload           (every user interaction)
//	"ping":  —                       (keepalive)
type Inbound struct {
	Type      string            `json:"type"`
	Component string            `json:"component,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	Name      string            `json:"name,omitempty"`
	Payload   Payload           `json:"payload,omitempty"`
	// Token, when present on a join, identifies a previous session
	// the server may have parked at disconnect. When the token
	// matches a live parked entry within its TTL, the server
	// resumes that session (preserving Filter/NewTitle/etc.);
	// otherwise it falls through to a fresh Mount.
	Token string `json:"token,omitempty"`
	// Path is the URL the client is navigating to (type
	// "navigate"). Server resolves it via Engine.RegisterRoute
	// to find the new component, swaps def in-session, and
	// emits a fresh "joined" frame.
	Path string `json:"path,omitempty"`
}

// Outbound is the discriminated union of all server → client
// messages. Dispatch by Type. Server frames map naturally to client
// reducer cases:
//
//	"joined":  Rendered             (full tree; client stores it)
//	"diff":    Diff                 (sparse patch; client merges)
//	"error":   Msg                  (handler returned an error)
//	"push":    Event, EventPayload  (server-initiated client event)
//	"reload":  —                    (client should hard-refresh)
//	"pong":    —
type Outbound struct {
	Type         string    `json:"type"`
	Rendered     *Rendered `json:"r,omitempty"`
	Diff         Diff      `json:"d,omitempty"`
	Msg          string    `json:"msg,omitempty"`
	Event        string    `json:"event,omitempty"`
	EventPayload any       `json:"payload,omitempty"`
	// Token accompanies "joined" frames. The client stores it in
	// memory and sends it back in the next join on reconnect; the
	// server uses it to find a parked session and resume state.
	// Rotated on every join (resumption included) so leaking a
	// token only enables one reconnect, not perpetual hijack.
	Token string `json:"token,omitempty"`
	// Path, when present on "joined", tells the client the URL
	// has changed (live-navigate). The client applies
	// history.pushState so the address bar reflects the new
	// location without a full reload.
	Path string `json:"path,omitempty"`
	// Style + Scope ship the new component's scoped CSS body
	// (already rewritten with the scope attribute prefix) and
	// the scope ID. Set on "joined" frames after live-navigate
	// so the client can swap the <head>'s style tag and the
	// mount container's data-nl-scope attribute together — the
	// SSR shell sets these up on first paint, but a navigate
	// swaps the body without touching the head, so the new
	// component's styles never land without this.
	Style string `json:"style,omitempty"`
	Scope string `json:"scope,omitempty"`

	// Stream-op frame fields ("stream-op" type). Stream is the
	// container name (matches nl-stream="X" on a DOM element);
	// Op is "append" / "prepend" / "delete" / "update" / "reset";
	// ID is the child's DOM id (required for delete/update);
	// HTML is the rendered child markup for append/prepend/update.
	Stream string `json:"stream,omitempty"`
	Op     string `json:"op,omitempty"`
	ID     string `json:"id,omitempty"`
	HTML   string `json:"html,omitempty"`
}
