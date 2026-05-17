package template

import "context"

// StreamRef is the handle a component uses to push incremental
// updates into an nl-stream-marked container without re-rendering
// the surrounding template. Operations are sent as individual
// "stream-op" frames; the client applies them to the DOM by
// finding the element with nl-stream="<name>" and mutating its
// children by ID.
//
// Trade-off vs nl-for:
//   - nl-for re-renders the whole list on every change and
//     diff-patches. Cheap for small lists; quadratic on large.
//   - nl-stream skips the re-render. Each op is O(1) on the
//     wire and O(child-count) for delete/update lookups in the
//     client. Right for chat feeds, logs, infinite-scroll lists.
//
// Use through ctx.Stream("<name>") inside a Mount or event
// handler. The name must match the nl-stream attribute on the
// container in the template.
//
// Items must carry a stable DOM id (id="..." attribute) so
// Delete and Update can find them. The Stream API doesn't
// enforce this — emit ids in your rendered HTML.
type StreamRef struct {
	name    string
	send    func(Outbound)
	context context.Context
}

// Append adds a child to the end of the stream container.
// The HTML should be a single root element carrying id="<id>"
// (the client uses the id for later delete/update lookups).
func (s *StreamRef) Append(id, html string) {
	s.send(Outbound{Type: "stream-op", Stream: s.name, Op: "append", ID: id, HTML: html})
}

// Prepend adds a child to the start of the stream container.
// Same id/html requirements as Append.
func (s *StreamRef) Prepend(id, html string) {
	s.send(Outbound{Type: "stream-op", Stream: s.name, Op: "prepend", ID: id, HTML: html})
}

// Delete removes the child whose DOM id matches id. No-op on
// the client when no element with that id exists in the
// container — covers the race where two tabs delete the same
// item concurrently.
func (s *StreamRef) Delete(id string) {
	s.send(Outbound{Type: "stream-op", Stream: s.name, Op: "delete", ID: id})
}

// Update replaces the child with id by the new HTML. Append-
// style fallback if the id isn't present (client appends);
// callers that want strict update-only behavior should check
// before calling.
func (s *StreamRef) Update(id, html string) {
	s.send(Outbound{Type: "stream-op", Stream: s.name, Op: "update", ID: id, HTML: html})
}

// Reset removes every child of the stream container. Useful
// for "switch dataset" flows where the previous items are no
// longer relevant.
func (s *StreamRef) Reset() {
	s.send(Outbound{Type: "stream-op", Stream: s.name, Op: "reset"})
}
