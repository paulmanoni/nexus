package mail

import (
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// BindFromConfig binds a marker type T to the [mail.<name>] block in
// nexus.toml — the mail counterpart to db.BindFromConfig, so log-in-dev /
// smtp-in-prod becomes a config edit with no build closure to hand-write. T
// must embed *Manager, exactly as with Bind; lifecycle + dashboard
// registration are identical.
//
//	mail.BindFromConfig[Mailer]("smtp", mail.WithDefault())
//
// reads the block:
//
//	[mail.smtp]
//	driver       = "smtp"           # "log" (default, sends nothing) | "smtp"
//	host         = "smtp.example.com"
//	port         = 587              # 0 → 587 (starttls) or 465 (tls)
//	username     = "apikey"
//	password     = "${SMTP_PASSWORD}"
//	encryption   = "starttls"       # none | starttls | tls
//	from_address = "no-reply@example.com"
//	from_name    = "Example"
//	timeout      = "30s"
//
// The build runs at boot (nexus.Get resolves the toml base layer, any config
// extension, and ENV overrides), so this works under nexus.Boot. Required
// fields for the chosen driver are still validated at boot by buildMailer.
func BindFromConfig[T any](name string, opts ...BindOption) nexus.Option {
	return Bind[T](name, func() Config { return configFromTOML(name) }, opts...)
}

func configFromTOML(name string) Config {
	p := "mail." + name + "."
	return Config{
		Driver:      config.Get(p+"driver", ""),
		Host:        config.Get(p+"host", ""),
		Port:        config.Get(p+"port", 0),
		Username:    config.Get(p+"username", ""),
		Password:    config.Get(p+"password", ""),
		Encryption:  config.Get(p+"encryption", ""),
		FromAddress: config.Get(p+"from_address", ""),
		FromName:    config.Get(p+"from_name", ""),
		Timeout:     config.Get(p+"timeout", time.Duration(0)),
	}
}
