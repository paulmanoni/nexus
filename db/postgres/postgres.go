// Package postgres links gorm's Postgres driver (pgx) into nexus/db.
// Blank-import it to enable Driver "postgres":
//
//	_ "github.com/paulmanoni/nexus/db/postgres"
package postgres

import (
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/paulmanoni/nexus/db"
)

func init() {
	db.RegisterDriver(db.Postgres, func(dsn string) gorm.Dialector { return postgres.Open(dsn) })
}
