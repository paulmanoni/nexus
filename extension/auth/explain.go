package auth

import (
	"fmt"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
)

// Explain checks s as boot would and describes the setup it gives: schemes
// in the order they are tried, the default gate, areas, endpoints and the
// session, throttle and password rules. `nexus auth check` prints it.
func Explain(s Settings) (string, error) {
	rs, err := resolveSettings(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	if rs.public {
		w(`default    public — endpoints are open unless gated ([auth] default = "public")`)
	} else {
		w(`default    signed-in — every endpoint needs a sign-in unless auth.Public()`)
	}
	if rs.cache > 0 {
		w("cache      Users.Load kept %s per user", rs.cache)
	} else {
		w("cache      off — Users.Load on every request")
	}
	w("\nschemes (tried in this order)")
	for _, sc := range rs.schemes {
		switch sc.Type {
		case SchemeSession:
			cookie := sc.Cookie
			if cookie == "" {
				cookie = "nexus_session"
			}
			w("  %-12s session   cookie %s%s", sc.name, cookie, map[bool]string{true: ", Secure", false: ""}[sc.Secure])
		case SchemeBearer:
			refresh := "no refresh tokens"
			if sc.Refresh > 0 {
				refresh = "refresh " + sc.Refresh.String()
			}
			w("  %-12s bearer    Authorization: Bearer <opaque>, ttl %s, %s", sc.name, sc.TTL, refresh)
		case SchemeAPIKey:
			w("  %-12s apikey    header %s", sc.name, sc.Header)
		case SchemeJWT:
			key := map[bool]string{true: "secret (HS256)", false: ""}[sc.Secret != ""]
			if sc.PublicKey != "" {
				key = "public_key (RS256/ES256)"
			}
			if sc.JWKS != "" {
				key = "jwks " + sc.JWKS
			}
			w("  %-12s jwt       Authorization: Bearer <JWT>, %s, subject %q%s%s", sc.name, key, sc.Subject,
				optional(", issuer ", sc.Issuer), optional(", audience ", sc.Audience))
		}
	}
	w("\npages")
	w("  login %s, home %s, forbidden %s, next via ?%s=", orNone(rs.login), rs.home, orNone(rs.forbidden), rs.nextParam)
	if rs.pageProp != "" {
		w("  Inertia pages get {user, can} as %q", rs.pageProp)
	}
	if len(rs.areas) > 0 {
		w("\nareas")
		for _, a := range rs.areas {
			kinds := "any signed-in user"
			if len(a.Kinds) > 0 {
				kinds = "kinds " + strings.Join(a.Kinds, ", ")
			}
			w("  %-12s %s  %s; login %s, home %s, forbidden %s", a.name, a.Prefix, kinds, orNone(a.Login), a.Home, orNone(a.Forbidden))
		}
	}
	ep := rs.endpoints
	if ep.Login+ep.Logout+ep.Me+ep.Token+ep.Revoke+rs.impersonation.Endpoint != "" {
		w("\nendpoints")
		for _, e := range [][2]string{{"POST", ep.Login}, {"POST", ep.Logout}, {"GET", ep.Me}, {"POST", ep.Token}, {"POST", ep.Revoke}, {"POST+DELETE", rs.impersonation.Endpoint}} {
			if e[1] != "" {
				w("  %-11s %s", e[0], e[1])
			}
		}
	}
	w("\nrules")
	w("  sessions   single %v, end on password change %v, idle %s", rs.sessions.Single, rs.endOnPw, orNever(rs.sessions.Idle))
	w("  throttle   account %s, ip %s, lockout %s", limitString(rs.throttle.account), limitString(rs.throttle.ip), rs.throttle.lockout)
	ids := make([]string, 0, len(rs.hashers.All))
	for _, h := range rs.hashers.All {
		ids = append(ids, h.ID())
	}
	w("  passwords  hashers %s (new: %s)", strings.Join(ids, ", "), rs.hashers.Default.ID())
	w("  impersonate with %q", rs.impersonation.Permission)
	return b.String(), nil
}

func optional(prefix, v string) string {
	if v == "" {
		return ""
	}
	return prefix + v
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

func orNever(d time.Duration) string {
	if d <= 0 {
		return "never"
	}
	return d.String()
}

func limitString(l limit) string {
	if l.n == 0 {
		return "off"
	}
	return fmt.Sprintf("%d per %s", l.n, l.window)
}

// ExplainTOML is Explain for the [auth] table of a nexus.toml document.
func ExplainTOML(raw []byte) (string, error) {
	s, err := config.DecodeTable(raw, "auth", Settings{Default: "signed-in", Cache: defaultCacheTTL})
	if err != nil {
		return "", err
	}
	return Explain(s)
}
