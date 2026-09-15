// Package mysql links gorm's MySQL driver into nexus/db. Blank-import
// it to enable Driver "mysql":
//
//	_ "github.com/paulmanoni/nexus/db/mysql"
package mysql

import (
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/paulmanoni/nexus/db"
)

func init() {
	db.RegisterDriver(db.MySQL, func(dsn string) gorm.Dialector { return mysql.Open(dsn) })
}
