package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	conn, _, err := d.DialContext(ctx, "ws://127.0.0.1:8080/__nexus/events", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			return
		}
		var e map[string]any
		_ = json.Unmarshal(data, &e)
		fmt.Printf("id=%v trace=%v kind=%v method=%v path=%v status=%v dur=%vms msg=%v\n",
			e["id"], e["traceId"], e["kind"], e["method"], e["path"], e["status"], e["durationMs"], e["message"])
	}
}