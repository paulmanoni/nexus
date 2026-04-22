package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"nexus"
	"nexus/trace"
)

func main() {
	app := nexus.New(
		nexus.WithTracing(1000),
		nexus.WithDashboard(),
		nexus.WithDashboardName("Petstore"),
	)

	pets := app.Service("pets").Describe("Pet inventory")

	pets.REST("GET", "/pets").
		Describe("List all pets").
		Handler(func(c *gin.Context) {
			// Simulate a downstream call so the trace shows a child event.
			start := time.Now()
			time.Sleep(5 * time.Millisecond)
			trace.Record(c, "db.pets.list", start, nil)

			c.JSON(http.StatusOK, gin.H{"pets": []string{"Rex", "Whiskers"}})
		})

	pets.REST("POST", "/pets").
		Describe("Create a pet").
		Handler(func(c *gin.Context) {
			c.JSON(http.StatusCreated, gin.H{"ok": true})
		})

	pets.WebSocket("/pets/stream").
		Describe("Echo channel for pet events").
		OnMessage(func(conn *websocket.Conn, t int, data []byte) error {
			return conn.WriteMessage(t, data)
		}).
		Mount()

	for _, e := range app.Registry().Endpoints() {
		fmt.Printf("registered: %-10s %s %s\n", e.Transport, e.Method, e.Path)
	}

	_ = app.Run(":8080")
}
