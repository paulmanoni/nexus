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
}
