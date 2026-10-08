package orm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"

	"github.com/paulmanoni/nexus/orm/internal/schema"
)

// declared is every manager orm.For made and every extension
// CreateExtension declared, for the migration planner: the models a
// program links are the models of its schema.
var declared struct {
	mu   sync.Mutex
	list []declaredModel
	exts []extension
}

type declaredModel struct {
	db        string
	unmanaged bool
	tables    func(Dialect) ([]schema.Table, error)
}

type extension struct{ db, name string }

// CreateExtension declares a Postgres extension the database needs —
// "vector" for Vector columns and vector indexes, "pg_trgm" for trigram
// indexes — Django's CreateExtension: nexus makemigrations writes it into
// the next migration (CreateTables makes it too). Declare it
// package-level, beside the models, where the planner finds it; passed to
// nexus.Boot as well, it fails boot while the database lacks it. Nothing
// installs an extension otherwise. orm.On names the database.
//
//	var Vectors = orm.CreateExtension("vector")
func CreateExtension(name string, opts ...ForOption) nexus.Option {
	var c forConfig
	for _, o := range opts {
		o(&c)
	}
	declared.mu.Lock()
	declared.exts = append(declared.exts, extension{c.db, name})
	declared.mu.Unlock()
	return nexus.Setup(func(ctx context.Context, app *nexus.App) error {
		d, err := waitConnected(ctx, &binding{app: app}, c.db)
		if err != nil {
			return err
		}
		return missingExtensions(ctx, d, []string{name})
	})
}

func declaredExtensions() []extension {
	declared.mu.Lock()
	defer declared.mu.Unlock()
	return slices.Clone(declared.exts)
}

func createExtension(d Dialect, name string) string {
	return "CREATE EXTENSION IF NOT EXISTS " + d.Quote(name)
}

// missingExtensions fails, naming how to install them, when a Postgres
// database lacks some of the extensions names.
func missingExtensions(ctx context.Context, d *DB, names []string) error {
	if d.dialect.Name() != "postgres" || len(names) == 0 {
		return nil
	}
	rows, err := d.sql.QueryContext(ctx, "SELECT extname FROM pg_extension")
	if err != nil {
		return err
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		have[n] = true
	}
	var errs []error
	for _, n := range names {
		if !have[n] {
			errs = append(errs, fmt.Errorf("orm: the database lacks the extension %q: add orm.CreateExtension(%q) to a migration (declare it, then nexus makemigrations)", n, n))
		}
	}
	return errors.Join(append(errs, rows.Err())...)
}

// snapshot is the schema a migration leaves: its tables and extensions.
// One with no extensions is written as the list of its tables, as before
// there were any.
type snapshot struct {
	Extensions []string       `json:"extensions"`
	Tables     []schema.Table `json:"tables"`
}

func declare(d declaredModel) {
	declared.mu.Lock()
	declared.list = append(declared.list, d)
	declared.mu.Unlock()
}

// MigrationStep is one statement of a planned migration, and a note when
// it needs a person (a drop that loses data, a change SQLite can't make;
// a note alone when there is no statement to write).
type MigrationStep struct {
	SQL  string `json:"sql,omitempty"`
	Note string `json:"note,omitempty"`
}

// MigrationPlan is the migration from a snapshot to the models a program
// links: the statements, and the snapshot to keep for the next one.
type MigrationPlan struct {
	Steps    []MigrationStep `json:"steps"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// PlanMigration is the migration from snapshot (the schema the last one
// left, empty for none) to the managed models of the databases dbs (""
// the default) declared with orm.For in the program, written for driver
// (postgres, mysql, sqlite). It is what nexus makemigrations runs; apps
// don't call it.
func PlanMigration(snap []byte, driver string, dbs ...string) (MigrationPlan, error) {
	var old snapshot
	if len(bytes.TrimSpace(snap)) > 0 {
		var err error
		if bytes.TrimSpace(snap)[0] == '[' {
			err = json.Unmarshal(snap, &old.Tables)
		} else {
			err = json.Unmarshal(snap, &old)
		}
		if err != nil {
			return MigrationPlan{}, fmt.Errorf("orm: reading the schema snapshot: %w", err)
		}
	}
	if len(dbs) == 0 {
		dbs = []string{""}
	}
	declared.mu.Lock()
	list := append([]declaredModel(nil), declared.list...)
	declared.mu.Unlock()
	dl := DialectFor(driver)
	var cur snapshot
	for _, e := range declaredExtensions() {
		if slices.Contains(dbs, e.db) && !slices.Contains(cur.Extensions, e.name) {
			cur.Extensions = append(cur.Extensions, e.name)
		}
	}
	at := map[string]int{}
	for _, m := range list {
		if m.unmanaged || !slices.Contains(dbs, m.db) {
			continue
		}
		ts, err := m.tables(dl)
		if err != nil {
			return MigrationPlan{}, err
		}
		for _, t := range ts {
			i, ok := at[t.Name]
			if !ok {
				at[t.Name] = len(cur.Tables)
				cur.Tables = append(cur.Tables, t)
				continue
			}
			if cur.Tables[i], err = mergeTable(cur.Tables[i], t); err != nil {
				return MigrationPlan{}, err
			}
		}
	}
	d := schema.Dialect{Name: dl.Name(), Quote: dl.Quote}
	var plan MigrationPlan
	for _, e := range cur.Extensions {
		if !slices.Contains(old.Extensions, e) && dl.Name() == "postgres" {
			plan.Steps = append(plan.Steps, MigrationStep{SQL: createExtension(dl, e)})
		}
	}
	for _, e := range old.Extensions {
		if !slices.Contains(cur.Extensions, e) {
			plan.Steps = append(plan.Steps, MigrationStep{Note: "the extension " + e + " is no longer declared: drop it by hand once nothing uses it"})
		}
	}
	for _, s := range d.Diff(old.Tables, cur.Tables) {
		plan.Steps = append(plan.Steps, MigrationStep{SQL: s.SQL, Note: s.Note})
	}
	cur.Tables = schema.Sort(cur.Tables)
	var out any = cur
	if len(cur.Extensions) == 0 {
		out = cur.Tables
	}
	var err error
	plan.Snapshot, err = json.MarshalIndent(out, "", "  ")
	return plan, err
}

// mergeTable is one table two models map: the columns and foreign keys
// of both, which must agree where they overlap.
func mergeTable(a, b schema.Table) (schema.Table, error) {
	cols := map[string]schema.Column{}
	for _, c := range a.Columns {
		cols[c.Name] = c
	}
	for _, c := range b.Columns {
		if have, ok := cols[c.Name]; ok {
			if have != c {
				return a, fmt.Errorf("orm: two models map table %s and disagree on its column %s: make one orm.Unmanaged()", a.Name, c.Name)
			}
			continue
		}
		a.Columns = append(a.Columns, c)
	}
	for _, fk := range b.FKs {
		if !slices.Contains(a.FKs, fk) {
			a.FKs = append(a.FKs, fk)
		}
	}
	for _, ix := range b.Indexes {
		i := slices.IndexFunc(a.Indexes, func(have schema.Index) bool { return have.Name == ix.Name })
		switch {
		case i < 0:
			a.Indexes = append(a.Indexes, ix)
		case a.Indexes[i] != ix:
			return a, fmt.Errorf("orm: two models map table %s and disagree on its index %s: make one orm.Unmanaged()", a.Name, ix.Name)
		}
	}
	return a, nil
}

// MigrateOption tunes Migrate.
type MigrateOption func(*migrateConfig)

type migrateConfig struct{ db, dir string }

// MigrateOn applies the migrations to the database db.Bind registered as
// name; the default database otherwise.
func MigrateOn(name string) MigrateOption { return func(c *migrateConfig) { c.db = name } }

// MigrateDir reads the migrations from dir within the file system; its
// root otherwise.
func MigrateDir(dir string) MigrateOption { return func(c *migrateConfig) { c.dir = dir } }

// Migrate applies the migrations in fsys at boot, after the databases
// connect and before the app serves: the NNNN_name.sql files nexus
// makemigrations writes, each one not yet recorded in the database's
// nexus_migrations table, in name order. A migration runs in a
// transaction where the database can roll DDL back (Postgres, SQLite;
// MySQL commits each statement). Replicas booting together take turns
// under a lock (Postgres advisory, MySQL GET_LOCK).
//
//	//go:embed migrations
//	var migrations embed.FS
//
//	nexus.Boot(db.BindFromConfig[DB]("main"), orm.Migrate(migrations, orm.MigrateDir("migrations")), Users)
func Migrate(fsys fs.FS, opts ...MigrateOption) nexus.Option {
	var c migrateConfig
	for _, o := range opts {
		o(&c)
	}
	return nexus.Setup(func(ctx context.Context, app *nexus.App) error {
		d, err := waitConnected(ctx, &binding{app: app}, c.db)
		if err != nil {
			return err
		}
		dir := fsys
		if c.dir != "" {
			if dir, err = fs.Sub(fsys, c.dir); err != nil {
				return err
			}
		}
		applied, err := ApplyMigrations(ctx, d, dir)
		for _, name := range applied {
			app.Logger().Info("orm: migration applied", "migration", name, "database", c.db)
		}
		return err
	})
}

// waitConnected is the database name once it connects: databases connect
// in the background as the app starts, and the migrations come first.
func waitConnected(ctx context.Context, b *binding, name string) (*DB, error) {
	deadline := time.Now().Add(MigrateWait)
	for {
		d, err := b.lookup(name)
		if err == nil || !errors.Is(err, nexus.Unavailable) || time.Now().After(deadline) {
			return d, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// MigrateWait is how long Migrate waits for its database to connect.
var MigrateWait = 30 * time.Second

// migrationName is a migration file's name: NNNN_words.sql.
var migrationName = regexp.MustCompile(`^\d+_[A-Za-z0-9_]+\.sql$`)

const migrationsTable = "nexus_migrations"

// ApplyMigrations applies the migrations in fsys (its top-level .sql
// files) that db hasn't recorded, as Migrate does at boot, and returns
// the names applied. For tools and tests.
func ApplyMigrations(ctx context.Context, db *DB, fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if !migrationName.MatchString(e.Name()) {
			return nil, fmt.Errorf("orm: migration %s: name it NNNN_words.sql", e.Name())
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	c, err := db.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	unlock, err := lockMigrations(ctx, c, db.dialect)
	if err != nil {
		return nil, err
	}
	defer unlock()

	d := schema.Dialect{Name: db.dialect.Name(), Quote: db.dialect.Quote}
	record := schema.Table{Name: migrationsTable, Columns: []schema.Column{
		{Name: "name", Kind: schema.String, Size: 255, PK: true},
		{Name: "checksum", Kind: schema.String, Size: 64},
		{Name: "applied_at", Kind: schema.Time},
	}}
	if _, err := c.ExecContext(ctx, d.Create(record, true)[0]); err != nil {
		return nil, fmt.Errorf("orm: creating %s: %w", migrationsTable, err)
	}
	done := map[string]string{}
	rows, err := c.QueryContext(ctx, "SELECT "+d.Quote("name")+", "+d.Quote("checksum")+" FROM "+d.Quote(migrationsTable))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n, sum string
		if err := rows.Scan(&n, &sum); err != nil {
			rows.Close()
			return nil, err
		}
		done[n] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var applied []string
	for _, name := range names {
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return applied, err
		}
		h := sha256.Sum256(src)
		sum := hex.EncodeToString(h[:])
		if prev, ok := done[name]; ok {
			if prev != sum {
				slog.WarnContext(ctx, "orm: an applied migration was edited; the edit is not applied", "migration", name)
			}
			continue
		}
		if err := applyOne(ctx, c, db.dialect, d, name, sum, splitSQL(string(src), db.dialect.Name() == "mysql")); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

func applyOne(ctx context.Context, c *sql.Conn, dl Dialect, d schema.Dialect, name, sum string, stmts []string) error {
	type execer interface {
		ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	}
	var run execer = c
	var tx *sql.Tx
	if dl.Name() != "mysql" {
		var err error
		if tx, err = c.BeginTx(ctx, nil); err != nil {
			return err
		}
		run = tx
	}
	fail := func(err error) error {
		if tx != nil {
			_ = tx.Rollback()
		}
		return err
	}
	for _, s := range stmts {
		if _, err := run.ExecContext(ctx, s); err != nil {
			return fail(fmt.Errorf("orm: migration %s: %w\n%s", name, err, s))
		}
	}
	ins := "INSERT INTO " + d.Quote(migrationsTable) + " (" + d.Quote("name") + ", " + d.Quote("checksum") + ", " + d.Quote("applied_at") + ") VALUES (" + dl.Placeholder(1) + ", " + dl.Placeholder(2) + ", " + dl.Placeholder(3) + ")"
	if _, err := run.ExecContext(ctx, ins, name, sum, time.Now().UTC()); err != nil {
		return fail(fmt.Errorf("orm: recording migration %s: %w", name, err))
	}
	if tx != nil {
		return tx.Commit()
	}
	return nil
}

// migrationLock is the Postgres advisory lock key of the migrations:
// "NXMIGRAT" as a big-endian int64.
const migrationLock = 0x4e58_4d49_4752_4154

func lockMigrations(ctx context.Context, c *sql.Conn, d Dialect) (func(), error) {
	switch d.Name() {
	case "postgres":
		if _, err := c.ExecContext(ctx, "SELECT pg_advisory_lock($1)", int64(migrationLock)); err != nil {
			return nil, fmt.Errorf("orm: locking migrations: %w", err)
		}
		return func() {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", int64(migrationLock))
		}, nil
	case "mysql":
		var got sql.NullInt64
		if err := c.QueryRowContext(ctx, "SELECT GET_LOCK('nexus_migrations', 600)").Scan(&got); err != nil {
			return nil, fmt.Errorf("orm: locking migrations: %w", err)
		}
		if got.Int64 != 1 {
			return nil, fmt.Errorf("orm: locking migrations: another process held the lock for 10 minutes")
		}
		return func() { _, _ = c.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK('nexus_migrations')") }, nil
	}
	return func() {}, nil
}

// splitSQL is a migration's statements: split at semicolons outside
// quotes, comments and Postgres dollar quotes; comments and blank
// statements dropped.
func splitSQL(src string, backslash bool) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	lexSQL(src, backslash, &b, func(i int) int {
		if src[i] == ';' {
			flush()
		} else {
			b.WriteByte(src[i])
		}
		return i + 1
	})
	flush()
	return out
}

// lexSQL writes src to b: quoted strings and identifiers and Postgres
// dollar-quoted bodies whole, comments dropped (a block comment as a
// space), and each other byte through code, which writes it or what it
// stands for and returns where to go on. backslash says a backslash
// escapes a quote inside a string (MySQL); elsewhere it is an ordinary
// character.
func lexSQL(src string, backslash bool, b *strings.Builder, code func(i int) int) {
	for i := 0; i < len(src); {
		ch := src[i]
		switch {
		case ch == '-' && strings.HasPrefix(src[i:], "--"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				i = len(src)
			} else {
				i += end
			}
			continue
		case ch == '/' && strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
			b.WriteByte(' ')
			continue
		case ch == '\'' || ch == '"' || ch == '`':
			j := i + 1
			for j < len(src) {
				if src[j] == ch {
					if j+1 < len(src) && src[j+1] == ch {
						j += 2
						continue
					}
					break
				}
				if backslash && ch != '`' && src[j] == '\\' && j+1 < len(src) {
					j++
				}
				j++
			}
			end := min(j+1, len(src))
			b.WriteString(src[i:end])
			i = end
			continue
		case ch == '$':
			if tag := dollarTag(src[i:]); tag != "" {
				end := strings.Index(src[i+len(tag):], tag)
				stop := len(src)
				if end >= 0 {
					stop = i + len(tag) + end + len(tag)
				}
				b.WriteString(src[i:stop])
				i = stop
				continue
			}
		}
		i = code(i)
	}
}

var dollarRE = regexp.MustCompile(`^\$[A-Za-z_]*\$`)

func dollarTag(s string) string { return dollarRE.FindString(s) }

// MigrationFile is a planned migration as nexus makemigrations writes it:
// the statements each ending with a semicolon, notes as comments.
func MigrationFile(steps []MigrationStep) []byte {
	var b strings.Builder
	for _, s := range steps {
		if s.Note != "" {
			for _, line := range strings.Split(s.Note, "\n") {
				b.WriteString("-- NOTE: " + line + "\n")
			}
		}
		if s.SQL != "" {
			b.WriteString(s.SQL + ";\n")
		}
		b.WriteString("\n")
	}
	return []byte(strings.TrimRight(b.String(), "\n") + "\n")
}

// PlanEnv is the variable nexus makemigrations sets to run an app as its
// planner, holding a JSON PlanRequest.
const PlanEnv = "NEXUS_ORM_PLAN"

// PlanRequest is what nexus makemigrations asks the planner: the snapshot
// file (empty for none), the driver, the databases, and the file to write
// the MigrationPlan to.
type PlanRequest struct {
	Snapshot string   `json:"snapshot"`
	Driver   string   `json:"driver"`
	DBs      []string `json:"dbs"`
	Out      string   `json:"out"`
}

// ServePlan makes the program nexus makemigrations' planner when PlanEnv
// is set: it writes the plan of the models the program links and exits,
// before main runs. The CLI calls it from an init it overlays into the
// app's main package; apps don't.
func ServePlan() {
	raw := os.Getenv(PlanEnv)
	if raw == "" {
		return
	}
	err := func() error {
		var r PlanRequest
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return err
		}
		var snap []byte
		if r.Snapshot != "" {
			var err error
			if snap, err = os.ReadFile(r.Snapshot); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		plan, err := PlanMigration(snap, r.Driver, r.DBs...)
		if err != nil {
			return err
		}
		out, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		return os.WriteFile(r.Out, out, 0o600)
	}()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}
