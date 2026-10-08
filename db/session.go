package db

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Session settings ride the connection string, so the driver applies them
// on every connection the pool opens — a SET through *gorm.DB would reach
// one pooled connection and miss the rest:
//
//   - MySQL: extra DSN parameters, which go-sql-driver runs as one
//     `SET name=value, …` per new connection.
//   - Postgres: unknown keywords, which pgx sends as run-time parameters in
//     the startup message.
//   - SQLite: `_pragma=name(value)` parameters, run on every new connection.

var sessionKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// reservedSessionKeys are the keys a driver reads as its own connection
// options instead of passing them to the server, plus the ones nexus sets
// from other Config fields.
var reservedSessionKeys = map[Driver][]string{
	MySQL: {
		"allowAllFiles", "allowCleartextPasswords", "allowFallbackToPlaintext",
		"allowNativePasswords", "allowOldPasswords", "charset", "checkConnLiveness",
		"clientFoundRows", "collation", "columnsWithAlias", "compress",
		"connectionAttributes", "interpolateParams", "loc", "maxAllowedPacket",
		"multiStatements", "parseTime", "readTimeout", "rejectReadOnly",
		"serverPubKey", "strict", "timeTruncate", "timeout", "tls", "writeTimeout",
	},
	Postgres: {
		"host", "port", "database", "dbname", "user", "password", "passfile",
		"connect_timeout", "sslmode", "sslkey", "sslcert", "sslrootcert",
		"sslnegotiation", "sslpassword", "sslsni", "krbspn", "krbsrvname",
		"target_session_attrs", "service", "servicefile", "min_protocol_version",
		"max_protocol_version", "channel_binding", "statement_cache_capacity",
		"description_cache_capacity", "default_query_exec_mode", "timezone",
	},
	SQLite: {},
}

// Validate reports a setting the driver can't take: interpolate_params on a
// driver other than MySQL, or a Session key the driver would not pass to
// the server as a session setting.
func (c Config) Validate() error {
	if c.InterpolateParams != nil && c.Driver != MySQL {
		return fmt.Errorf("db: interpolate_params is MySQL's, not %s's", c.Driver)
	}
	reserved := reservedSessionKeys[c.Driver]
	for _, k := range sessionKeys(c.Session) {
		if !sessionKey.MatchString(k) {
			return fmt.Errorf("db: session setting %q is not a valid name", k)
		}
		for _, r := range reserved {
			if strings.EqualFold(k, r) {
				if strings.EqualFold(k, "timezone") {
					return fmt.Errorf("db: session setting %q: use the timezone key of the database instead", k)
				}
				return fmt.Errorf("db: session setting %q is a %s connection option, not a server setting", k, c.Driver)
			}
		}
	}
	return nil
}

func sessionKeys(s map[string]string) []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (c Config) mysqlSession() string {
	var b strings.Builder
	for _, k := range sessionKeys(c.Session) {
		b.WriteString("&" + k + "=" + url.QueryEscape(sqlLiteral(c.Session[k])))
	}
	return b.String()
}

func (c Config) postgresSession() string {
	var b strings.Builder
	for _, k := range sessionKeys(c.Session) {
		b.WriteString(" " + k + "=" + pgDSNValue(c.Session[k]))
	}
	return b.String()
}

func (c Config) sqliteDSN() string {
	if len(c.Session) == 0 {
		return c.Database
	}
	sep := "?"
	if strings.Contains(c.Database, "?") {
		sep = "&"
	}
	var b strings.Builder
	b.WriteString(c.Database)
	for _, k := range sessionKeys(c.Session) {
		v := c.Session[k]
		if !bareWord.MatchString(v) {
			// SQLite's string: a quote doubled. A backslash escapes
			// nothing there, so MySQL's \' would end the string and
			// run the rest as statements on every connection.
			v = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		b.WriteString(sep + "_pragma=" + url.QueryEscape(k+"("+v+")"))
		sep = "&"
	}
	return b.String()
}

var (
	bareWord = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
	number   = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)
)

// sqlLiteral is a value as MySQL's SET takes it: a number or DEFAULT as
// is, anything else as a quoted string — which boolean and enum variables
// accept too ('OFF', 'NO_ENGINE_SUBSTITUTION').
func sqlLiteral(v string) string {
	if strings.EqualFold(v, "DEFAULT") {
		return "DEFAULT"
	}
	if number.MatchString(v) {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}
