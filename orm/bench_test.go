package orm_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/paulmanoni/nexus/v2/db"

	"github.com/paulmanoni/nexus/orm"
)

// The benchmarks compare the ORM with database/sql by hand and with GORM,
// on the same in-memory SQLite table: what the ORM's reflection and SQL
// building cost over the driver, and how it stands against GORM.

type benchUser struct {
	ID        int64 `gorm:"primaryKey"`
	CreatedAt time.Time
	Name      string
	Email     string
	Age       int
	Active    bool
}

func (benchUser) TableName() string { return "bench_users" }

var BenchUsers = orm.For[benchUser]()

const benchSchema = `CREATE TABLE bench_users (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at DATETIME,
	name TEXT, email TEXT, age INTEGER, active BOOLEAN)`

type benchEnv struct {
	ctx  context.Context
	sql  *sql.DB
	gorm *gorm.DB
}

func benchOpen(b *testing.B, rows int) benchEnv {
	b.Helper()
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(m.Stop)
	g := m.GetDB()
	s, _ := g.DB()
	if _, err := s.Exec(benchSchema); err != nil {
		b.Fatal(err)
	}
	ctx := orm.WithDB(context.Background(), orm.Open(s, "sqlite"))
	if rows > 0 {
		batch := make([]*benchUser, rows)
		for i := range batch {
			batch[i] = &benchUser{Name: fmt.Sprint("user", i), Email: fmt.Sprint("u", i, "@mail.com"), Age: 18 + i%50, Active: i%2 == 0}
		}
		if err := BenchUsers.BulkCreate(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
	return benchEnv{ctx: ctx, sql: s, gorm: g}
}

func BenchmarkInsert(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			if _, err := e.sql.ExecContext(e.ctx, `INSERT INTO bench_users (created_at, name, email, age, active) VALUES (?, ?, ?, ?, ?)`,
				time.Now(), "ali", fmt.Sprint(i), 25, true); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			u := benchUser{Name: "ali", Email: fmt.Sprint(i), Age: 25, Active: true}
			if err := BenchUsers.Create(e.ctx, &u); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for i := 0; b.Loop(); i++ {
			u := benchUser{Name: "ali", Email: fmt.Sprint(i), Age: 25, Active: true}
			if err := e.gorm.Create(&u).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkBulkInsert1000(b *testing.B) {
	rows := func() []*benchUser {
		out := make([]*benchUser, 1000)
		for i := range out {
			out[i] = &benchUser{Name: "ali", Email: fmt.Sprint(i), Age: 25}
		}
		return out
	}
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for b.Loop() {
			if err := BenchUsers.BulkCreate(e.ctx, rows()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 0)
		for b.Loop() {
			if err := e.gorm.CreateInBatches(rows(), 500).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkSelect1000(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			rows, err := e.sql.QueryContext(e.ctx, `SELECT id, created_at, name, email, age, active FROM bench_users`)
			if err != nil {
				b.Fatal(err)
			}
			var out []benchUser
			for rows.Next() {
				var u benchUser
				if err := rows.Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active); err != nil {
					b.Fatal(err)
				}
				out = append(out, u)
			}
			rows.Close()
			if len(out) != 1000 {
				b.Fatal(len(out))
			}
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			var out []benchUser
			if err := e.gorm.Find(&out).Error; err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
}

func BenchmarkGetByID(b *testing.B) {
	b.Run("sql", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			var u benchUser
			if err := e.sql.QueryRowContext(e.ctx, `SELECT id, created_at, name, email, age, active FROM bench_users WHERE id = ?`, 1+i%1000).
				Scan(&u.ID, &u.CreatedAt, &u.Name, &u.Email, &u.Age, &u.Active); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			if _, err := BenchUsers.Get(e.ctx, orm.Q{"id": 1 + i%1000}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for i := 0; b.Loop(); i++ {
			var u benchUser
			if err := e.gorm.First(&u, 1+i%1000).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkFilterCount(b *testing.B) {
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			if _, err := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true, "name__icontains": "user1"}).Count(e.ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			var n int64
			if err := e.gorm.Model(&benchUser{}).Where("age >= ? AND active = ? AND LOWER(name) LIKE LOWER(?)", 30, true, "%user1%").Count(&n).Error; err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkValuesNames(b *testing.B) {
	b.Run("orm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			out, err := BenchUsers.Values[string]("name").All(e.ctx)
			if err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
	b.Run("gorm", func(b *testing.B) {
		e := benchOpen(b, 1000)
		for b.Loop() {
			var out []string
			if err := e.gorm.Model(&benchUser{}).Pluck("name", &out).Error; err != nil || len(out) != 1000 {
				b.Fatal(len(out), err)
			}
		}
	})
}

// BenchmarkBuild is the ORM alone: a QuerySet chained and its SQL built,
// no database.
func BenchmarkBuild(b *testing.B) {
	e := benchOpen(b, 0)
	_ = e
	for b.Loop() {
		q := BenchUsers.Filter(orm.Q{"age__gte": 30, "active": true}).Exclude(orm.Q{"name__icontains": "x"}).OrderBy("-age", "name").Limit(20)
		_ = q
	}
}
