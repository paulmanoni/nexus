package view

import (
	"context"
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
	subs map[string]map[*Socket]struct{}
}{subs: map[string]map[*Socket]struct{}{}}

// Subscribe makes the connected page receive what is broadcast to the
// topics: its Info runs, then it re-renders. Call it from Mount; on the
// first, server-rendered load (Connected() false) it does nothing.
func (s *Socket) Subscribe(topics ...string) {
	if s == nil || !s.connected {
		return
	}
	hub.Lock()
	defer hub.Unlock()
	for _, t := range topics {
		if hub.subs[t] == nil {
			hub.subs[t] = map[*Socket]struct{}{}
		}
		hub.subs[t][s] = struct{}{}
		s.topics = append(s.topics, t)
	}
}

// close unsubscribes the page from everything, when its connection ends.
func (s *Socket) close() {
	hub.Lock()
	for _, t := range s.topics {
		delete(hub.subs[t], s)
		if len(hub.subs[t]) == 0 {
			delete(hub.subs, t)
		}
	}
	s.topics = nil
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
	targets := make([]*Socket, 0, len(hub.subs[topic]))
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
