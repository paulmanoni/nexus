package nexus

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
)

// A stopped app leaves no dev-reload poller or watcher running.
func TestDevReloadStopsWithApp(t *testing.T) {
	count := func() int {
		buf := make([]byte, 1<<22)
		n := runtime.Stack(buf, true)
		return strings.Count(string(buf[:n]), "nexus.mountDevReload.func")
	}
	before := count()
	dir := t.TempDir()
	t.Setenv(dev.Env, "1")
	t.Setenv(dev.RootEnv, dir)
	for i := 0; i < 3; i++ {
		fsys := fstest.MapFS{"web/dist/index.html": {Data: []byte("<html>x</html>")}}
		_, stop, err := InProcess(config.Runtime{}, ServeFrontend(fsys, "web/dist"))
		if err != nil {
			t.Fatal(err)
		}
		if err := stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for count() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count() - before; n > 0 {
		t.Errorf("%d dev-reload goroutines outlived their stopped apps", n)
	}
}
