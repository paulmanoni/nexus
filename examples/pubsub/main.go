// Demonstrates nexus's typed pub/sub primitive end-to-end:
//
//   - A package-level Topic[T] declared once, publishable from anywhere.
//   - A REST endpoint that publishes to the topic on each request.
//   - Two independent subscriptions on the same topic, registered as
//     module options. Each one becomes a worker the dashboard surfaces
//     under "pubsub:<topic>:<subscription>".
//   - pubsub.UseInMemory() — zero-broker default, suitable for tests
//     and `nexus dev` runs. Production swaps in pubsub.UseRabbit(...).
//
// Run with: go run ./examples/pubsub
// Then:     curl -X POST localhost:8080/adopt -d '{"petId":"42","ownerId":"7"}'
// Watch the logs — both subscribers fire for each adoption event.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/pubsub"
)

// PetAdopted carries the fields downstream subscribers care about.
// Topic payloads are JSON-encoded; struct field tags work as you'd
// expect.
type PetAdopted struct {
	PetID   string `json:"petId"`
	OwnerID string `json:"ownerId"`
}

// PetAdoptedTopic is the package-level handle. Declared once so every
// publisher and subscriber refers to the same topic identity. The
// transport is not bound until pubsub.UseInMemory() runs at boot.
var PetAdoptedTopic = pubsub.NewTopic[PetAdopted]("pet-adopted", pubsub.TopicConfig{
	Description: "Emitted whenever a pet is adopted by an owner.",
})

// --- Adoption module: the publisher --------------------------------

type AdoptionService struct{}

func NewAdoptionService() *AdoptionService { return &AdoptionService{} }

func (s *AdoptionService) Adopt(c *httpx.Ctx) {
	var body PetAdopted
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, httpx.H{"error": err.Error()})
		return
	}
	if err := PetAdoptedTopic.Publish(c.Request.Context(), body); err != nil {
		c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, httpx.H{"ok": true})
}

var adoptionModule = nexus.Module("adoption",
	nexus.Provide(NewAdoptionService),
	nexus.Invoke(func(app *nexus.App, svc *AdoptionService) {
		s := app.Service("adoption").Describe("Records pet adoptions")
		s.REST("POST", "/adopt").Describe("Record an adoption event").Handler(svc.Adopt)
	}),
)

// --- Notification module: subscriber #1 ----------------------------
//
// Sends a "your new pet is on the way" message to the owner. Failures
// are retried up to MaxRetries before the message lands in the DLQ.

var notificationsModule = nexus.Module("notifications",
	pubsub.Subscribe(PetAdoptedTopic, "send-welcome-email",
		func(ctx context.Context, e PetAdopted) error {
			log.Printf("notifications: welcome email queued for owner=%s pet=%s", e.OwnerID, e.PetID)
			return nil
		},
		pubsub.SubscriptionConfig{MaxRetries: 5}),
)

// --- Audit module: subscriber #2 -----------------------------------
//
// Independent of notifications. Demonstrates fan-out: both
// subscriptions on the same topic receive every published event.

var auditModule = nexus.Module("audit",
	pubsub.Subscribe(PetAdoptedTopic, "audit-log",
		func(ctx context.Context, e PetAdopted) error {
			b, _ := json.Marshal(e)
			log.Printf("audit: pet-adopted %s", b)
			return nil
		},
		pubsub.SubscriptionConfig{}),
)

// --- Boot ----------------------------------------------------------

func main() {
	nexus.Run(
		nexus.Config{
			Server:        nexus.ServerConfig{Addr: ":8080"},
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Pubsub Demo"},
			TraceCapacity: 500,
		},
		// Bind the in-memory transport to every registered topic.
		// Swap for pubsub.UseRabbit(...) in production.
		pubsub.UseInMemory(),
		adoptionModule,
		notificationsModule,
		auditModule,
	)
}
