package view

import (
	"context"
	"slices"
	"sync"
)

// Message is what a live page's Info receives: a topic and the data
// broadcast to it, as the publisher's Go value.
type Message struct {
	Topic string
	Data  any
}

// liveInbox bounds how many undelivered messages a page holds; past it, a
// broadcast is dropped for that page — a busy page renders the latest state
// with its next message anyway.
const liveInbox = 64

var hub = struct {
	sync.Mutex
	subs map[string]map[*socket]struct{}
}{subs: map[string]map[*socket]struct{}{}}

// subscribe makes in — the page, or a view it embeds — hear what is
// broadcast to the topics: its Info runs, then the page re-renders. On the
// first, server-rendered load it does nothing.
func (s *socket) subscribe(in *instance, topics ...string) {
	if s == nil || !s.connected {
		return
	}
	if s.subs == nil {
		s.subs = map[string][]*instance{}
	}
	hub.Lock()
	defer hub.Unlock()
	for _, t := range topics {
		if slices.Contains(s.subs[t], in) {
			continue
		}
		s.subs[t] = append(s.subs[t], in)
		if hub.subs[t] == nil {
			hub.subs[t] = map[*socket]struct{}{}
		}
		if _, ok := hub.subs[t][s]; !ok {
			hub.subs[t][s] = struct{}{}
			s.topics = append(s.topics, t)
		}
	}
}

// drop ends what in subscribed to and tracked: an embedded view the page
// stopped rendering.
func (s *socket) drop(in *instance) {
	if s == nil {
		return
	}
	hub.Lock()
	for t, ins := range s.subs {
		ins = slices.DeleteFunc(ins, func(x *instance) bool { return x == in })
		if len(ins) > 0 {
			s.subs[t] = ins
			continue
		}
		delete(s.subs, t)
		delete(hub.subs[t], s)
		if len(hub.subs[t]) == 0 {
			delete(hub.subs, t)
		}
		s.topics = slices.DeleteFunc(s.topics, func(x string) bool { return x == t })
	}
	hub.Unlock()
	kept := s.presences[:0]
	for _, p := range s.presences {
		if p.in == in {
			leave(p)
			continue
		}
		kept = append(kept, p)
	}
	s.presences = kept
}

// close unsubscribes the page from everything, when its connection ends.
func (s *socket) close() {
	hub.Lock()
	for _, t := range s.topics {
		delete(hub.subs[t], s)
		if len(hub.subs[t]) == 0 {
			delete(hub.subs, t)
		}
	}
	s.topics, s.subs = nil, nil
	hub.Unlock()
	for _, u := range s.uploads {
		u.clear()
	}
	s.uploads = nil
	s.untrackAll()
}

// Broadcast sends data to every live page subscribed to topic — from an
// event, a handler, a job, anywhere. Each page runs its Info and re-renders.
// It reports how many pages on this replica it reached; with a Relay
// (UseRelay) the pages of the other replicas get it too.
func Broadcast(ctx context.Context, topic string, data any) int {
	n := deliver(topic, data)
	relay(topic, data)
	return n
}

// deliver hands data to the pages on this replica subscribed to topic.
func deliver(topic string, data any) int {
	hub.Lock()
	targets := make([]*socket, 0, len(hub.subs[topic]))
	for s := range hub.subs[topic] {
		targets = append(targets, s)
	}
	hub.Unlock()
	n := 0
	for _, s := range targets {
		select {
		case s.inbox <- Message{Topic: topic, Data: data}:
			n++
		default:
		}
	}
	return n
}

func subscribers(topic string) int {
	hub.Lock()
	defer hub.Unlock()
	return len(hub.subs[topic])
}
