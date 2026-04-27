package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/trace"
)

func main() {
	app := nexus.New(nexus.Config{
		Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Petstore"},
		TraceCapacity: 1000,
	})

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

	app.Cron("refresh-inventory", "@every 30s").
		Describe("Refresh the pet cache from upstream").
		Service("pets").
		Handler(func(ctx context.Context) error {
			time.Sleep(40 * time.Millisecond)
			return nil
		})

	app.Cron("daily-report", "0 9 * * *").
		Describe("Email the daily sales report at 09:00").
		Service("pets").
		Handler(func(ctx context.Context) error { return nil })

	for _, e := range app.Registry().Endpoints() {
		fmt.Printf("registered: %-10s %s %s\n", e.Transport, e.Method, e.Path)
	}

	_ = app.Run(":8080")
}
