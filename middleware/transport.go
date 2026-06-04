package middleware

import "strings"

// Transport identifies which wire protocol a request arrived on.
type Transport uint8

const (
	TransportREST Transport = iota
	TransportGraphQL
	TransportWebSocket
)

func (t Transport) String() string {
	switch t {
	case TransportREST:
		return "REST"
	case TransportGraphQL:
		return "GraphQL"
	case TransportWebSocket:
		return "WebSocket"
	default:
		return "unknown"
	}
}

// TransportSet is a bitset over Transport. The zero value is the empty set.
type TransportSet uint8

func bit(t Transport) TransportSet { return 1 << t }

// Transports builds a set from its members.
func Transports(ts ...Transport) TransportSet {
	var s TransportSet
	for _, t := range ts {
		s |= bit(t)
	}
	return s
}

// AllTransports is the common declaration for write-once middleware.
var AllTransports = Transports(TransportREST, TransportGraphQL, TransportWebSocket)

// Has reports whether t is a member of the set.
func (s TransportSet) Has(t Transport) bool { return s&bit(t) != 0 }

// String renders the set for fail-closed error messages, e.g. "{REST, GraphQL}".
func (s TransportSet) String() string {
	var parts []string
	for _, t := range []Transport{TransportREST, TransportGraphQL, TransportWebSocket} {
		if s.Has(t) {
			parts = append(parts, t.String())
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
