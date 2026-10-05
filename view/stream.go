package view

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/a-h/templ"
)

// Stream is a list a live page shows without keeping it, as LiveView's
// streams: the page sends what changed — rows inserted, replaced, deleted —
// and the browser keeps the rest. A chat, a feed or a log of thousands of
// rows costs the server only the latest change.
//
//	type Chat struct {
//	    Messages view.Stream[Message]
//	}
//
//	func (c *Chat) Mount(ctx context.Context, sock *view.Socket, store *Store) error {
//	    c.Messages.Configure(func(m Message) string { return "msg-" + m.ID })
//	    c.Messages.Limit(-200)            // the browser keeps the last 200
//	    c.Messages.Reset(store.Recent(50)...)
//	    sock.Subscribe("chat")
//	    return nil
//	}
//
//	func (c *Chat) Info(ctx context.Context, msg view.Message) error {
//	    var m Message
//	    if err := msg.Decode(&m); err != nil {
//	        return err
//	    }
//	    c.Messages.Insert(m)
//	    return nil
//	}
//
//	templ (c *Chat) Render() {
//	    <ul { c.Messages.Attrs()... }>
//	        for _, m := range c.Messages.Items() {
//	            <li id={ c.Messages.ID(m) }>{ m.Text }</li>
//	        }
//	    </ul>
//	}
//
// Each item renders with the id Configure gives it, as a direct child of
// the element Attrs is spread on. Items are the items of the latest change
// only; the browser inserts them (or replaces the element with the same
// id), deletes what Delete named, and empties the list on Reset.
//
// A Stream is page state like an Assign. Use it in Mount, Params, Info and
// events — the page's goroutine.
type Stream[T any] struct {
	id     func(T) string
	limit  int
	prefix string // tells this stream's changes apart from another instance's
	seq    int
	batch  []streamItem[T]
	del    []string
	reset  bool
	shown  bool // the batch was rendered: the next change starts a new one

	t   *tracker
	bit uint64
	ver uint64
}

type streamItem[T any] struct {
	item T
	id   string
	at   int // -1: the end; else the index
}

// Configure gives the stream how to identify an item: its element's id.
func (s *Stream[T]) Configure(id func(T) string) { s.id = id }

// Limit bounds how many items the browser keeps: a positive n keeps the
// first n, a negative n the last -n, 0 all.
func (s *Stream[T]) Limit(n int) { s.limit = n }

// Insert adds items at the end — or, for an item whose id is shown already,
// replaces it in place.
func (s *Stream[T]) Insert(items ...T) {
	s.change()
	for _, it := range items {
		s.put(it, -1)
	}
}

// Prepend adds items at the start, in order.
func (s *Stream[T]) Prepend(items ...T) {
	s.change()
	for i, it := range items {
		s.put(it, i)
	}
}

// InsertAt adds item at index i.
func (s *Stream[T]) InsertAt(i int, item T) {
	s.change()
	s.put(item, max(i, 0))
}

// Delete removes items from the list.
func (s *Stream[T]) Delete(items ...T) {
	for _, it := range items {
		s.DeleteID(s.ID(it))
	}
}

// DeleteID removes the item with the element id id.
func (s *Stream[T]) DeleteID(id string) {
	s.change()
	for i, b := range s.batch {
		if b.id == id {
			s.batch = append(s.batch[:i], s.batch[i+1:]...)
			break
		}
	}
	s.del = append(s.del, id)
}

// Reset empties the list, then shows items.
func (s *Stream[T]) Reset(items ...T) {
	s.change()
	s.batch, s.del, s.reset = nil, nil, true
	for _, it := range items {
		s.put(it, -1)
	}
}

// Items are the items of the latest change, in order: render each with its
// ID as a direct child of the element Attrs is spread on.
func (s *Stream[T]) Items() []T {
	s.read()
	if s.t != nil && s.t.rec != nil {
		s.t.rec.streamLoop()
	}
	out := make([]T, len(s.batch))
	for i, b := range s.batch {
		out[i] = b.item
	}
	return out
}

// ID is item's element id.
func (s *Stream[T]) ID(item T) string {
	if s.id == nil {
		panic("view.Stream: Configure it with the items' ids before using it")
	}
	return s.id(item)
}

// Attrs are the list element's attributes: spread them on it.
func (s *Stream[T]) Attrs() templ.Attributes {
	s.read()
	s.shown = true
	ops := streamOps{Reset: s.reset, Delete: s.del, Limit: s.limit}
	for _, b := range s.batch {
		ops.Insert = append(ops.Insert, [2]any{b.id, b.at})
	}
	j, _ := json.Marshal(ops)
	return templ.Attributes{
		"data-nx-stream":     "",
		"data-nx-stream-seq": s.prefix + "-" + strconv.Itoa(s.seq),
		"data-nx-stream-ops": string(j),
	}
}

type streamOps struct {
	Reset  bool     `json:"r,omitempty"`
	Delete []string `json:"d,omitempty"`
	Insert [][2]any `json:"i,omitempty"`
	Limit  int      `json:"l,omitempty"`
}

// change starts a new change once the last one has been rendered: the
// browser applies each change once.
func (s *Stream[T]) change() {
	if s.prefix == "" {
		var b [4]byte
		_, _ = rand.Read(b[:])
		s.prefix = hex.EncodeToString(b[:])
	}
	if s.shown || s.seq == 0 {
		s.batch, s.del, s.reset, s.shown = nil, nil, false, false
		s.seq++
	}
	if s.t != nil {
		s.ver = s.t.bump()
	} else {
		s.ver++
	}
}

func (s *Stream[T]) put(item T, at int) {
	id := s.ID(item)
	for i, b := range s.batch {
		if b.id == id {
			s.batch[i] = streamItem[T]{item, id, b.at}
			return
		}
	}
	s.batch = append(s.batch, streamItem[T]{item, id, at})
}

func (s *Stream[T]) read() {
	if s.t != nil && s.t.rec != nil {
		s.t.rec.read(s.bit)
	}
}

// flush lets go of a change once the browser has been sent it: the server
// keeps no rows.
func (s *Stream[T]) flush() {
	if s.shown {
		s.batch, s.del, s.reset = nil, nil, false
	}
}

func (s *Stream[T]) bindAssign(t *tracker, bit uint64) { s.t, s.bit = t, bit }
func (s *Stream[T]) version() uint64                   { return s.ver }
