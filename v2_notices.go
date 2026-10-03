package nexus

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"unicode"

	"github.com/paulmanoni/nexus/internal/v2notice"
)

// v1.80 bridge release: what nexus 2.0 has in place of each v1 API the app
// uses. The APIs themselves call v2notice.Called; under `nexus dev` the uses
// are printed as one block once the app has booted (see Run). Outside dev
// each call costs one atomic load.
const (
	v2LoadConfig      = "config.Load (package nexus/config) — or nexus.Boot, which loads nexus.toml itself"
	v2MustLoadConfig  = "config.MustLoad (package nexus/config) — or nexus.Boot, which loads nexus.toml itself"
	v2RunConfig       = "nexus.Config is config.Runtime (package nexus/config); nexus.Run stays, but nexus.Boot with settings in nexus.toml is the documented entry"
	v2Get             = "config.Get (package nexus/config), same signature"
	v2MustGet         = "config.MustGet (package nexus/config), same signature"
	v2ServeFrontend   = "nexus.Frontend(fs, root, opts…), same arguments; it also provides the page shell as *nexus.Document"
	v2AsCRUD          = "removed: declare a nexus.Resource[T](prefix) with Index/Show/Create/Update/Destroy actions"
	v2AsRestHandler   = "nexus.AsRest with a handler that takes *httpx.Ctx"
	v2Errors          = "nexus.Invalid().Field(name, msg).Global(msg), a nexus.Error with code InvalidInput"
	v2MapCRUDError    = "nexus.ErrorOf / nexus.CodeOf: every error maps through one table per transport"
	v2ErrForbidden    = "return nexus.Forbidden (an error code of the nexus.Error model)"
	v2Global          = "nexus.Middleware(…), an option whose middleware may take DI parameters and declares its stage"
	v2IsDev           = "dev.Enabled (package nexus/dev)"
	v2PreserveDev     = "dev.Preserve (package nexus/dev), same arguments"
	v2PreserveDevJSON = "dev.PreserveJSON (package nexus/dev), same arguments"
	v2DevStateDir     = "dev.StateDir (package nexus/dev)"
	v2NewNotifier     = "notify.New (package nexus/notify)"
	v2Dotenv          = "drop the option: nexus.Boot loads .env itself — list files with [runtime] dotenv = [\".env\"] (\"!.env\" makes one required)"
	v2ErrorOption     = "nexus.FailBoot(err) — nexus.Error becomes the error type"
	v2GenerateDriver  = "removed: nothing reads the Generate driver slot"
	v2AppFromGin      = "removed: inertia, view and dashboard routes find the app themselves"
	v2Managed         = "the build func takes *slog.Logger (the framework logs with log/slog)"
	v2WithClientIP    = "removed: the framework records the caller's address on every request; read it with nexus.ClientIP(ctx)"
	v2ClientIPFromCtx = "nexus.ClientIP(ctx)"
	v2MaxBody         = "request bodies are capped at 32 MB by default; set [runtime.server] max_body_bytes (or nexus.MaxBody per route) if the app accepts larger ones"
	v2UnknownKeys     = "nexus.toml is strict: an unknown or misplaced key fails boot (the keys are listed above); `nexus migrate v2` moves misplaced ones"
)

// noteGlobalMiddleware records Config.Middleware.Global (read by New).
func noteGlobalMiddleware(cfg Config) {
	if len(cfg.Middleware.Global) > 0 {
		v2notice.Note("Config.Middleware.Global", v2Global)
	}
}

// noteChangedDefaults records the v1 defaults the app relies on that 2.0
// changes.
func noteChangedDefaults(cfg Config) {
	if !v2notice.Enabled() {
		return
	}
	if cfg.Server.MaxBodyBytes == 0 {
		v2notice.Note("[runtime.server] max_body_bytes unset (v1: no limit)", v2MaxBody)
	}
}

// noteErrForbidden records a handler or Authorize hook returning
// nexus.ErrForbidden. An Authorize refusal is wrapped with ErrForbidden by
// the framework, so only the app's own error counts there.
func noteErrForbidden(err error) {
	if err == nil || !v2notice.Enabled() {
		return
	}
	var fe forbiddenError
	if errors.As(err, &fe) {
		err = fe.err
	}
	if errors.Is(err, ErrForbidden) {
		v2notice.Note("nexus.ErrForbidden", v2ErrForbidden)
	}
}

// noteURITag records a `uri:` struct tag on a REST args type.
func noteURITag(t reflect.Type) {
	if !v2notice.Enabled() {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, ok := f.Tag.Lookup("uri")
		if !ok {
			continue
		}
		if _, hasPath := f.Tag.Lookup("path"); hasPath {
			continue
		}
		name = strings.Split(name, ",")[0]
		v2notice.Note(fmt.Sprintf("uri:%q tag on %s.%s", name, t, f.Name),
			fmt.Sprintf("path:%q — v2 binds path params from the path tag only", name))
	}
}

// noteNewOpName records a GraphQL op whose name v1 derives by dropping a New
// prefix from its handler (NewListPets → listPets). v2 names an op after the
// handler as written (newListPets), so the wire name changes unless the
// registration names it with nexus.Op. Handlers in nexus's own packages are
// skipped.
func noteNewOpName(fn any, v1Name string) {
	if !v2notice.Enabled() {
		return
	}
	rv := reflect.ValueOf(fn)
	if rv.Kind() != reflect.Func || rv.IsNil() {
		return
	}
	f := runtime.FuncForPC(rv.Pointer())
	if f == nil || v2notice.Framework(f.Name()) {
		return
	}
	bare := runtimeFuncName(rv)
	if !strings.HasPrefix(bare, "New") || len(bare) <= 3 || !unicode.IsUpper(rune(bare[3])) {
		return
	}
	v2Name := "new" + bare[3:]
	file, line := f.FileLine(f.Entry())
	v2notice.NoteAt(fmt.Sprintf("op %q from %s", v1Name, bare),
		fmt.Sprintf("the op is named after the handler as written, %q — add nexus.Op(%q) to the registration (or //nexus:use nexus.Op(%q) above an annotated handler) to keep the wire name", v2Name, v1Name, v1Name),
		fmt.Sprintf("%s:%d", v2notice.Rel(file), line))
}
