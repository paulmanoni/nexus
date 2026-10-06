package orm

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/trace"
)

func TestRepeatWarning(t *testing.T) {
	devMode = true
	t.Cleanup(func() { devMode = false })
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx, sp := trace.StartSpan(context.Background(), "request")
	defer sp.End(nil)
	for range RepeatWarn + 3 {
		noteRepeat(ctx, `SELECT * FROM "authors" WHERE "id" = $1`)
	}
	noteRepeat(ctx, `SELECT 1`)
	if n := strings.Count(logs.String(), "N+1"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "SelectRelated") {
		t.Fatalf("no hint: %s", logs.String())
	}
	logs.Reset()
	for range RepeatWarn {
		noteRepeat(context.Background(), `SELECT 2`)
	}
	if logs.Len() != 0 {
		t.Fatal("warned outside a request")
	}
}
