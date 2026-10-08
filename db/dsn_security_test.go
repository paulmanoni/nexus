package db

import (
	"testing"

	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// A password or database name holding any byte pgx ends a bare value at
// is quoted, so it can't add keywords to the connection string: a host
// sending the password elsewhere, an sslmode turning TLS off.
func TestPostgresDSNKeywordInjection(t *testing.T) {
	for _, sep := range []string{" ", "\t", "\n", "\r", "\v", "\f"} {
		cfg := Config{Driver: Postgres, Host: "db.internal", Port: "5432", User: "app",
			Password: "secret" + sep + "host=attacker.example" + sep + "sslmode=disable",
			Database: "shop" + sep + "options=-csearch_path=evil", SSLMode: "require"}
		pc, err := pgconn.ParseConfig(cfg.DSN())
		if err != nil {
			t.Fatalf("%q: %v", sep, err)
		}
		if pc.Host != "db.internal" || pc.Password != cfg.Password || pc.Database != cfg.Database || pc.TLSConfig == nil {
			t.Errorf("%q: the DSN gained keywords: host %q, password %q, database %q, tls %v", sep, pc.Host, pc.Password, pc.Database, pc.TLSConfig != nil)
		}
		if _, ok := pc.RuntimeParams["options"]; ok {
			t.Errorf("%q: options injected", sep)
		}
	}
}

// A SQLite pragma's value is a SQL string as SQLite reads one: a quote
// doubled, never escaped with a backslash, which SQLite doesn't know.
func TestSQLitePragmaQuoting(t *testing.T) {
	c := Config{Driver: SQLite, Database: "app.db", Session: map[string]string{"journal_mode": `wal'); ATTACH DATABASE 'x.db' AS x; --`}}
	want := "app.db?_pragma=" + "journal_mode%28%27wal%27%27%29%3B+ATTACH+DATABASE+%27%27x.db%27%27+AS+x%3B+--%27%29"
	if got := c.DSN(); got != want {
		t.Fatalf("DSN = %s\nwant %s", got, want)
	}
}

// A MySQL password or database name with the DSN's own punctuation can't
// add parameters (allowAllFiles, a tls downgrade) or move the address.
func TestMySQLDSNParameterInjection(t *testing.T) {
	for _, s := range []string{"p@ss/w:rd", "x?allowAllFiles=true&tls=false", "x)/y?multiStatements=true", "a@tcp(attacker:3306)/", "%2F?&=#'\"\\ \t\r\n"} {
		cfg := Config{Driver: MySQL, Host: "db.internal", Port: "3306", User: "app", Password: s, Database: "shop" + s}
		mc, err := mysqldrv.ParseDSN(cfg.DSN())
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if mc.Addr != "db.internal:3306" || mc.Passwd != s || mc.DBName != "shop"+s || mc.User != "app" || mc.AllowAllFiles || mc.MultiStatements {
			t.Errorf("%q: parsed as addr %q user %q password %q db %q allowAllFiles %v multiStatements %v", s, mc.Addr, mc.User, mc.Passwd, mc.DBName, mc.AllowAllFiles, mc.MultiStatements)
		}
	}
}
