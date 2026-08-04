package oauth2

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-oauth2/oauth2/v4/models"
)

// Two calls to defaultTokenStore stand in for two processes either side of
// a `nexus dev` rebuild: a token issued before must still resolve after.
func TestDefaultTokenStoreSurvivesARebuild(t *testing.T) {
	t.Setenv("NEXUS_DEV_STATE", filepath.Join(t.TempDir(), "state.json"))
	ctx := context.Background()

	before := defaultTokenStore()
	tok := &models.Token{
		ClientID:        "web",
		UserID:          "alice",
		Access:          "tok-abc",
		AccessCreateAt:  time.Now(),
		AccessExpiresIn: time.Hour,
	}
	if err := before.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}

	after := defaultTokenStore()
	got, err := after.GetByAccess(ctx, "tok-abc")
	if err != nil {
		t.Fatalf("GetByAccess after rebuild: %v", err)
	}
	if got == nil || got.GetUserID() != "alice" {
		t.Fatalf("token lost across the rebuild: %#v", got)
	}
}

// Outside nexus dev nothing is written and the store is per-process, so a
// production binary behaves exactly as it did before.
func TestDefaultTokenStoreIsMemoryOutsideDev(t *testing.T) {
	t.Setenv("NEXUS_DEV_STATE", "")
	ctx := context.Background()

	first := defaultTokenStore()
	if err := first.Create(ctx, &models.Token{
		ClientID:        "web",
		UserID:          "alice",
		Access:          "tok-xyz",
		AccessCreateAt:  time.Now(),
		AccessExpiresIn: time.Hour,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := defaultTokenStore().GetByAccess(ctx, "tok-xyz")
	if err == nil && got != nil {
		t.Fatal("a second store saw the first one's token — state leaked outside nexus dev")
	}
}
