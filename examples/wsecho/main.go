// Command wsecho is a runnable demo of nexus.AsWS: one WebSocket path
// (/events) with two typed message handlers (chat.send + chat.typing) sharing
// one connection pool. Open http://localhost:8080/__nexus/ to see the
// endpoint card, or dial /events with any WebSocket client:
//
//	websocat 'ws://localhost:8080/events?userId=alice'
//	> {"type":"chat.send","data":{"text":"hello"}}
//
// Every connection receives the echo — broadcast is automatic via sess.Emit.
package main

import (
	"github.com/paulmanoni/nexus"
)

// ChatService is a typed service wrapper so the dashboard's Architecture
// view groups the two handlers under one "chat" node.
type ChatService struct{ *nexus.Service }

func NewChatService(app *nexus.App) *ChatService {
	return &ChatService{app.Service("chat").Describe("Real-time chat")}
}

// ChatPayload is the typed args body decoded from the envelope's `data`
// field. Works for both message types in this demo.
type ChatPayload struct {
	Text string `json:"text"`
}

// NewChatSend broadcasts a typed chat.message to every connection using
// the session's envelope-aware Emit. The user id is taken from the
// upgrade-time identity (see `?userId=` in the dial URL above).
func NewChatSend(svc *ChatService, sess *nexus.WSSession, p nexus.Params[ChatPayload]) error {
	sess.Emit("chat.message", map[string]string{
		"text": p.Args.Text,
		"user": sess.UserID(),
	})
	return nil
}

// NewChatTyping emits a lightweight "someone is typing" signal to every
// connection. Separate registration so the framework dispatches by the
// envelope's `type` field — no manual switch on msg.Type in user code.
func NewChatTyping(svc *ChatService, sess *nexus.WSSession, p nexus.Params[ChatPayload]) error {
	sess.Emit("chat.typing", map[string]string{"user": sess.UserID()})
	return nil
}

func main() {
	nexus.Run(
		nexus.Config{
			Server:        nexus.ServerConfig{Addr: ":8080"},
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "WS Echo"},
			TraceCapacity: 1000,
		},
		nexus.Module("chat",
			nexus.Provide(NewChatService),
			nexus.AsWS("/events", "chat.send", NewChatSend),
			nexus.AsWS("/events", "chat.typing", NewChatTyping),
		),
	)
}