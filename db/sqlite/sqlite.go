// Package sqlite links the pure-Go SQLite engine (glebarez/modernc)
// into nexus/db. Blank-import it to enable Driver "sqlite":
//
//	_ "github.com/paulmanoni/nexus/db/sqlite"
//
// Kept out of nexus/db itself so a Postgres or MySQL app doesn't ship
// the transpiled-C SQLite engine (~5MB of binary) it never opens.
package sqlite

import (
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/paulmanoni/nexus/db"
)

func init() {
	db.RegisterDriver(db.SQLite, func(dsn string) gorm.Dialector { return sqlite.Open(dsn) })
}
