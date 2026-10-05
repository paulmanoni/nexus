package view

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"
)

// Presence is who is on a topic — the users on a page, the people in a
// room — as Phoenix Presence tracks it: each live page tracks itself while
// it is open, every page subscribed to the topic hears who joins and
// leaves, and with a Relay (UseRelay) it spans the app's replicas.
//
//	func (r *Room) Mount(ctx context.Context, sock *view.Socket, name string) error {
//	    me := auth.Current(ctx)
//	    sock.Subscribe("room:" + name)
//	    if sock.Connected() {
//	        sock.Track("room:"+name, me.ID, Seen{Name: me.User.Name, At: time.Now()})
//	    }
//	    r.Online.Set(view.Presences("room:" + name))
//	    return nil
//	}
//
//	func (r *Room) Info(ctx context.Context, msg view.Message) error {
//	    if _, ok := msg.Data.(view.PresenceDiff); ok {
//	        r.Online.Set(view.Presences(msg.Topic))
//	    }
//	    return nil
//	}
//
// A key is who is present (a user id); the same key can be present more
// than once — two tabs — each with its own meta. A page that ends leaves:
// its connection closes, or the reconnect grace (ResumeGrace) runs out. A
// replica that stops answering for PresenceTTL is taken to have left with
// everyone it had.

// PresenceMeta is what a page tracked a key with.
type PresenceMeta struct{ raw json.RawMessage }

// Decode reads the meta into v.
func (m PresenceMeta) Decode(v any) error { return json.Unmarshal(m.raw, v) }

// MarshalJSON writes the meta as it was tracked.
func (m PresenceMeta) MarshalJSON() ([]byte, error) {
	if m.raw == nil {
		return []byte("null"), nil
	}
	return m.raw, nil
}

// Presence is one key on a topic, with a meta per page that tracks it.
type Presence struct {
	Key   string
	Metas []PresenceMeta
}

// PresenceDiff is the Info message a topic's pages get when keys join or
// leave it: msg.Data.(view.PresenceDiff). A key in Leaves may still be
// present — only one of its pages left; Presences tells.
type PresenceDiff struct {
	Joins  []Presence
	Leaves []Presence
}

// PresenceTTL is how long a replica's presences last without word from it.
var PresenceTTL = 30 * time.Second

// presenceEvery is how often a replica restates its presences on a relay.
var presenceEvery = 10 * time.Second

// presenceTopic carries presence between replicas: pages never subscribe
// to it.
const presenceTopic = "nx:presence"

// entries: topic → key → ref (one tracking page) → meta.
type entries map[string]map[string]map[string]json.RawMessage

var presence = struct {
	sync.Mutex
	local  entries
	remote map[string]*remotePresence // by replica
	// gen counts changes; lists caches Presences by topic, built once per
	// change and shared by every page that asks.
	gen   uint64
	lists map[string]presenceList
}{local: entries{}, remote: map[string]*remotePresence{}, lists: map[string]presenceList{}}

type presenceList struct {
	gen  uint64
	list []Presence
}

type remotePresence struct {
	seen    time.Time
	entries entries
}

// presenceMsg travels between replicas.
type presenceMsg struct {
	Kind    string          `json:"k"`           // join, leave, state, hello
	Topic   string          `json:"t,omitempty"` // join, leave
	Key     string          `json:"key,omitempty"`
	Ref     string          `json:"ref,omitempty"`
	Meta    json.RawMessage `json:"m,omitempty"`
	Entries entries         `json:"e,omitempty"` // state: the replica's every presence
}

type tracked struct{ topic, key, ref string }

// Track makes the page present on topic as key, with meta (JSON-encoded),
// until it ends or Untrack. Pages subscribed to topic get a PresenceDiff.
// It does nothing in the first, HTTP render's Mount: only a connected page
// is present.
func (s *Socket) Track(topic, key string, meta any) {
	if !s.Connected() {
		return
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		log.Printf("nexus view: Track(%q, %q): meta is not JSON: %v", topic, key, err)
		raw = []byte("null")
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	ref := hex.EncodeToString(b[:])
	presence.Lock()
	put(presence.local, topic, key, ref, raw)
	presence.Unlock()
	s.presences = append(s.presences, tracked{topic, key, ref})
	deliver(topic, PresenceDiff{Joins: []Presence{{Key: key, Metas: []PresenceMeta{{raw}}}}})
	relayPresence(presenceMsg{Kind: "join", Topic: topic, Key: key, Ref: ref, Meta: raw})
}

// Untrack ends the page's presence as key on topic.
func (s *Socket) Untrack(topic, key string) {
	kept := s.presences[:0]
	for _, p := range s.presences {
		if p.topic == topic && p.key == key {
			leave(p)
			continue
		}
		kept = append(kept, p)
	}
	s.presences = kept
}

// untrackAll ends the page's presences: it ended.
func (s *Socket) untrackAll() {
	for _, p := range s.presences {
		leave(p)
	}
	s.presences = nil
}

func leave(p tracked) {
	presence.Lock()
	raw, ok := take(presence.local, p.topic, p.key, p.ref)
	presence.Unlock()
	if !ok {
		return
	}
	deliver(p.topic, PresenceDiff{Leaves: []Presence{{Key: p.key, Metas: []PresenceMeta{{raw}}}}})
	relayPresence(presenceMsg{Kind: "leave", Topic: p.topic, Key: p.key, Ref: p.ref})
}

// Presences are the keys present on topic across the replicas, sorted by
// key. The list is built once per change and shared by every page that
// asks: read it, don't change it.
func Presences(topic string) []Presence {
	presence.Lock()
	defer presence.Unlock()
	if c, ok := presence.lists[topic]; ok && c.gen == presence.gen {
		return c.list
	}
	byKey := map[string][]PresenceMeta{}
	add := func(e entries) {
		for key, refs := range e[topic] {
			for _, raw := range refs {
				byKey[key] = append(byKey[key], PresenceMeta{raw})
			}
		}
	}
	add(presence.local)
	for _, r := range presence.remote {
		add(r.entries)
	}
	out := make([]Presence, 0, len(byKey))
	for key, metas := range byKey {
		out = append(out, Presence{Key: key, Metas: metas})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(presence.lists) > 4096 {
		clear(presence.lists)
	}
	presence.lists[topic] = presenceList{presence.gen, out}
	return out
}

func put(e entries, topic, key, ref string, raw json.RawMessage) {
	presence.gen++
	if e[topic] == nil {
		e[topic] = map[string]map[string]json.RawMessage{}
	}
	if e[topic][key] == nil {
		e[topic][key] = map[string]json.RawMessage{}
	}
	e[topic][key][ref] = raw
}

func take(e entries, topic, key, ref string) (json.RawMessage, bool) {
	raw, ok := e[topic][key][ref]
	if !ok {
		return nil, false
	}
	presence.gen++
	delete(e[topic][key], ref)
	if len(e[topic][key]) == 0 {
		delete(e[topic], key)
	}
	if len(e[topic]) == 0 {
		delete(e, topic)
	}
	return raw, true
}

// relayPresence sends a presence change to the other replicas.
func relayPresence(m presenceMsg) {
	relay(presenceTopic, m)
}

// onRemotePresence takes a presence message from another replica.
func onRemotePresence(origin string, data json.RawMessage) {
	var m presenceMsg
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch m.Kind {
	case "hello":
		// A replica started: tell it who is here.
		statePresence()
		return
	case "join":
		presence.Lock()
		r := remoteOf(origin)
		put(r.entries, m.Topic, m.Key, m.Ref, m.Meta)
		presence.Unlock()
		deliver(m.Topic, PresenceDiff{Joins: []Presence{{Key: m.Key, Metas: []PresenceMeta{{m.Meta}}}}})
	case "leave":
		presence.Lock()
		r := remoteOf(origin)
		raw, ok := take(r.entries, m.Topic, m.Key, m.Ref)
		presence.Unlock()
		if ok {
			deliver(m.Topic, PresenceDiff{Leaves: []Presence{{Key: m.Key, Metas: []PresenceMeta{{raw}}}}})
		}
	case "state":
		if m.Entries == nil {
			m.Entries = entries{}
		}
		presence.Lock()
		r := remoteOf(origin)
		old := r.entries
		r.entries = m.Entries
		presence.gen++
		presence.Unlock()
		for topic, d := range diffEntries(old, m.Entries) {
			deliver(topic, d)
		}
	}
}

// remoteOf is origin's presences, noting that it was heard from;
// presence is locked.
func remoteOf(origin string) *remotePresence {
	r := presence.remote[origin]
	if r == nil {
		r = &remotePresence{entries: entries{}}
		presence.remote[origin] = r
	}
	r.seen = time.Now()
	return r
}

// diffEntries is what changed from old to new, by topic.
func diffEntries(old, new entries) map[string]PresenceDiff {
	out := map[string]PresenceDiff{}
	for topic, keys := range new {
		for key, refs := range keys {
			for ref, raw := range refs {
				if _, had := old[topic][key][ref]; !had {
					d := out[topic]
					d.Joins = append(d.Joins, Presence{Key: key, Metas: []PresenceMeta{{raw}}})
					out[topic] = d
				}
			}
		}
	}
	for topic, keys := range old {
		for key, refs := range keys {
			for ref, raw := range refs {
				if _, has := new[topic][key][ref]; !has {
					d := out[topic]
					d.Leaves = append(d.Leaves, Presence{Key: key, Metas: []PresenceMeta{{raw}}})
					out[topic] = d
				}
			}
		}
	}
	return out
}

// statePresence restates this replica's presences for the others.
func statePresence() {
	presence.Lock()
	snap := entries{}
	for topic, keys := range presence.local {
		for key, refs := range keys {
			for ref, raw := range refs {
				put(snap, topic, key, ref, raw)
			}
		}
	}
	presence.Unlock()
	relayPresence(presenceMsg{Kind: "state", Entries: snap})
}

// expirePresence drops the presences of replicas not heard from within
// PresenceTTL.
func expirePresence() {
	presence.Lock()
	gone := map[string]PresenceDiff{}
	for origin, r := range presence.remote {
		if time.Since(r.seen) < PresenceTTL {
			continue
		}
		for topic, d := range diffEntries(r.entries, entries{}) {
			g := gone[topic]
			g.Leaves = append(g.Leaves, d.Leaves...)
			gone[topic] = g
		}
		delete(presence.remote, origin)
		presence.gen++
	}
	presence.Unlock()
	for topic, d := range gone {
		deliver(topic, d)
	}
}

// runPresence keeps this replica's presence known on a relay: a hello on
// start, its state every presenceEvery, and the others' expiry.
func runPresence(ctx context.Context) {
	relayPresence(presenceMsg{Kind: "hello"})
	statePresence()
	tick := time.NewTicker(presenceEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			statePresence()
			expirePresence()
		}
	}
}
