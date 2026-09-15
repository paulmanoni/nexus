package mail

import (
	"fmt"
	"reflect"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/internal/bindutil"
	"github.com/paulmanoni/nexus/resource"
)

// Config declares a mailer. Driver selects the backend; the remaining
// fields are read per-driver (log uses only From*; smtp uses the whole
// connection set). Build it inline in Bind, typically pulling secrets from
// nexus.Get so they resolve from the environment / nexus.toml at boot.
type Config struct {
	// Driver is "smtp" or "log". Empty defaults to "log" — the safe dev
	// default that prints messages instead of sending them.
	Driver string

	// --- smtp ---
	Host       string
	Port       int    // defaults to 587 (starttls) or 465 (tls) when zero
	Username   string // empty → no auth
	Password   string
	Encryption string // "none" | "starttls" (default when Host set) | "tls"

	// --- common ---
	FromAddress string        // default sender address
	FromName    string        // optional default sender display name
	Timeout     time.Duration // dial/send deadline; 0 → 30s
}

// buildMailer turns a Config into a concrete Mailer, validating the
// required fields for the chosen driver.
func buildMailer(cfg Config) (Mailer, error) {
	from := Address{Name: cfg.FromName, Email: cfg.FromAddress}
	switch cfg.Driver {
	case "", "log":
		return &LogMailer{From: from}, nil
	case "smtp":
		if cfg.Host == "" {
			return nil, fmt.Errorf("mail: smtp driver requires Host")
		}
		enc := Encryption(cfg.Encryption)
		switch enc {
		case "":
			enc = EncryptionStartTLS // sensible submission default
		case EncryptionNone, EncryptionStartTLS, EncryptionTLS:
		default:
			return nil, fmt.Errorf("mail: unknown encryption %q (want \"none\", \"starttls\", or \"tls\")", cfg.Encryption)
		}
		port := cfg.Port
		if port == 0 {
			if enc == EncryptionTLS {
				port = 465
			} else {
				port = 587
			}
		}
		return &SMTPMailer{
			Host:       cfg.Host,
			Port:       port,
			Username:   cfg.Username,
			Password:   cfg.Password,
			Encryption: enc,
			From:       from,
			Timeout:    cfg.Timeout,
		}, nil
	default:
		return nil, fmt.Errorf("mail: unknown driver %q (want \"smtp\" or \"log\")", cfg.Driver)
	}
}

// Bind wires a named mailer declaratively — the mail counterpart to
// cache.Bind / storage.Bind / db.BindFromConfig. It lives in this
// extension (not the nexus root) so importing nexus never pulls net/smtp
// or this package in. T must embed *mail.Manager:
//
//	type Mailer struct{ *mail.Manager }
//
//	nexus.Run(cfg, mail.Bind[Mailer]("smtp", func() mail.Config {
//	    return mail.Config{
//	        Driver:      "smtp",
//	        Host:        nexus.Get[string]("mail.host"),
//	        Port:        nexus.Get[int]("mail.port", 587),
//	        Username:    nexus.Get[string]("mail.username"),
//	        Password:    nexus.Get[string]("mail.password"),
//	        FromAddress: "no-reply@example.com",
//	    }
//	}, mail.WithDefault()))
//
// build() runs in the DI constructor (so nexus.Get resolves), and the
// mailer is registered as a dashboard resource. Handlers inject *Mailer
// and call Send on it directly. A bad Config fails fast at boot.
func Bind[T any](name string, build func() Config, opts ...BindOption) nexus.Option {
	fieldIdx := embeddedManagerField[T]()
	if name == "" {
		panic("mail.Bind: name must not be empty")
	}
	if build == nil {
		panic("mail.Bind: build func must not be nil")
	}

	ctor := func() (*T, error) {
		mailer, err := buildMailer(build())
		if err != nil {
			return nil, err
		}
		return bindutil.NewHolder[T](fieldIdx, NewManager(mailer)), nil
	}

	// Options apply at register time — not option construction — matching
	// db.Bind, so config-derived options resolve lazily under nexus.Boot.
	register := func(app *nexus.App, h *T) {
		bc := bindutil.Apply(opts)
		m := bindutil.ManagerOf[*Manager](h, fieldIdx)
		var ropts []resource.Option
		if bc.AsDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(m.AsResource(name, bc.Description, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource. Alias of
// the shared binder option type so db/cache/mail/storage stay uniform.
type BindOption = bindutil.Option

// WithDefault marks this mailer as the default mail resource.
func WithDefault() BindOption {
	return func(c *bindutil.Options) { c.AsDefault = true }
}

// WithDescription overrides the dashboard resource description.
func WithDescription(s string) BindOption {
	return func(c *bindutil.Options) { c.Description = s }
}

func embeddedManagerField[T any]() int {
	return bindutil.EmbeddedField[T]("mail.Bind", reflect.TypeFor[*Manager](),
		"type M struct{ *mail.Manager }")
}
