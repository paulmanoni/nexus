package main

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/paulmanoni/nexus/orm"
)

// `nexus makemigrations` writes the next migration of the ORM's models
// (Django's makemigrations) as a Go file of the app's migrations package:
// the app is built with a tool overlaid into its main package and run
// (main never does) to replay the migrations it registers, compare them
// with the models it declares, and write the operations between.

func newMakeMigrationsCmd(stdout, stderr io.Writer) *cobra.Command {
	var o makeMigrationsOpts
	cmd := &cobra.Command{
		Use:   "makemigrations [name]",
		Short: "Write the next migration of the ORM's models, as Go",
		Long: `Write the next migration of the ORM's models, as Go.

The app is built and run as a tool (its main never runs): it replays the
migrations its migrations package registers into the schema they leave,
compares it with its models (the types embedding orm.Model; managed ones,
of the database; an orm.For or orm.Of of a plain struct is never planned),
and writes the operations between as <dir>/NNNN_<name>.go, a file of package migrations
(migrations/<db> for a database that isn't the default). Import the package
in the app so orm.Migrate (and nexus migrate) apply them.

A table or column gone where one alike appeared may have been renamed: on a
terminal makemigrations asks ("Did you rename users.mail to users.email?
[y/N]"); --noinput (and no terminal) answers no, writing a removal and an
addition with a note. Read the file before committing it: notes mark the
steps that need a person, and the file is yours to edit.

--empty writes a migration with an empty RunGo, for a data migration.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.name = args[0]
			}
			o.in = os.Stdin
			o.interactive = !o.noinput && term.IsTerminal(int(os.Stdin.Fd()))
			return runMakeMigrations(o, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&o.root, "root", ".", "project directory")
	cmd.Flags().StringVar(&o.db, "db", "", "the [databases.<db>] block (default: the default database)")
	cmd.Flags().StringVar(&o.dialect, "dialect", "", "postgres | mysql | sqlite (default: the block's driver)")
	cmd.Flags().StringVar(&o.dir, "dir", "", "migrations directory (default migrations, migrations/<db> for a database that isn't the default)")
	cmd.Flags().BoolVar(&o.check, "check", false, "fail when the models have changes no migration holds, writing nothing (CI)")
	cmd.Flags().BoolVar(&o.empty, "empty", false, "write a migration with an empty RunGo, for a data migration")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the migration instead of writing it")
	cmd.Flags().BoolVar(&o.noinput, "noinput", false, "ask nothing: a rename is written as a removal and an addition")
	return cmd
}

type makeMigrationsOpts struct {
	root, db, dialect, dir, name  string
	check, empty, dryRun, noinput bool
	interactive                   bool
	in                            io.Reader
}

func runMakeMigrations(o makeMigrationsOpts, stdout, stderr io.Writer) error {
	root := findModuleRoot(o.root)
	if root == "" {
		return fmt.Errorf("no go.mod at or above %s", o.root)
	}
	target, err := migrationTarget(root, o.db, o.dialect)
	if err != nil {
		return err
	}
	dir := o.dir
	if dir == "" {
		dir = migrationsDir(root, target)
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if o.name != "" && !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(o.name) {
		return fmt.Errorf("a migration name is letters, digits and _: %q", o.name)
	}
	next, err := nextMigration(dir)
	if err != nil {
		return err
	}
	pkg, err := migrationsPackage(dir, target)
	if err != nil {
		return err
	}
	req := orm.CLIRequest{Command: "plan", DB: target.ormName(), Models: target.dbs, Dialect: target.driver,
		Number: next, Name: o.name, Package: pkg, Empty: o.empty, Interactive: o.interactive}
	var res orm.PlanResult
	if err := runORMTool(root, req, o.in, stdout, stderr, &res); err != nil {
		return err
	}
	if len(res.Ops) == 0 {
		fmt.Fprintf(stdout, "No changes in the models of %s.\n", target.label())
		return nil
	}
	if o.check {
		fmt.Fprintf(stderr, "The models of %s have changes no migration holds:\n", target.label())
		for _, op := range res.Ops {
			fmt.Fprintf(stderr, "  - %s\n", op)
		}
		return errors.New("run nexus makemigrations")
	}
	path := filepath.Join(dir, res.File)
	if o.dryRun {
		fmt.Fprintf(stdout, "// %s\n%s", path, res.Source)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, res.Source, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	for _, op := range res.Ops {
		fmt.Fprintf(stdout, "  - %s\n", op)
	}
	for _, n := range res.Notes {
		if n != "" {
			fmt.Fprintf(stderr, "  ! %s\n", n)
		}
	}
	return nil
}

var migrationFileRE = regexp.MustCompile(`^(\d+)_[A-Za-z0-9_]+\.go$`)

// nextMigration is the number after the highest in dir.
func nextMigration(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	high := 0
	for _, e := range entries {
		if m := migrationFileRE.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			high = max(high, n)
		}
	}
	return high + 1, nil
}

// migrationsDir is where a database's migrations live: migrations for the
// default, migrations/<db> for another.
func migrationsDir(root string, t migrationTargetInfo) string {
	if t.isDefault {
		return filepath.Join(root, "migrations")
	}
	return filepath.Join(root, "migrations", t.name)
}

// migrationsPackage is the package of the migrations in dir: the one its
// files declare, else migrations for the default database's and the
// database's name (as a Go name) for another's.
func migrationsPackage(dir string, t migrationTargetInfo) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, parser.PackageClauseOnly)
			if err == nil {
				return f.Name.Name, nil
			}
		}
	}
	if t.isDefault {
		return "migrations", nil
	}
	name := regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(strings.ToLower(t.name), "_")
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		name = "m" + name
	}
	return name, nil
}

// migrationTargetInfo is the database a migration is for: its nexus.toml
// name, driver, and the orm.On names of its models.
type migrationTargetInfo struct {
	name, driver string
	isDefault    bool
	dbs          []string
}

// ormName is the database as the migrations name it: "" the default.
func (t migrationTargetInfo) ormName() string {
	if t.isDefault {
		return ""
	}
	return t.name
}

func (t migrationTargetInfo) label() string {
	if t.isDefault {
		return "the default database"
	}
	return "database " + t.name
}

// databaseBlocks is nexus.toml's [databases] blocks: their drivers, and
// the default's name ("" when none is marked and there are several).
func databaseBlocks(root string) (map[string]string, string, error) {
	var doc struct {
		Databases map[string]struct {
			Driver  string `toml:"driver"`
			Default bool   `toml:"default"`
		} `toml:"databases"`
	}
	if raw, err := os.ReadFile(filepath.Join(root, "nexus.toml")); err == nil {
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return nil, "", fmt.Errorf("nexus.toml: %w", err)
		}
	}
	drivers := map[string]string{}
	var names []string
	def := ""
	for n, d := range doc.Databases {
		drivers[n] = d.Driver
		names = append(names, n)
		if d.Default {
			def = n
		}
	}
	if def == "" && len(names) == 1 {
		def = names[0]
	}
	return drivers, def, nil
}

func migrationTarget(root, db, dialect string) (migrationTargetInfo, error) {
	drivers, def, err := databaseBlocks(root)
	if err != nil {
		return migrationTargetInfo{}, err
	}
	t := migrationTargetInfo{name: db, isDefault: db == "" || db == def}
	if t.isDefault {
		t.name = def
	} else if _, ok := drivers[db]; !ok && dialect == "" {
		return t, fmt.Errorf("nexus.toml has no [databases.%s] (or pass --dialect)", db)
	}
	t.driver = dialect
	if t.driver == "" && t.name != "" {
		t.driver = drivers[t.name]
	}
	switch t.driver {
	case "postgres", "postgresql", "pgx":
		t.driver = "postgres"
	case "mysql", "sqlite":
	case "":
		return t, errors.New("no dialect: give the database a driver in nexus.toml, or pass --dialect")
	default:
		return t, fmt.Errorf("dialect %q: postgres, mysql or sqlite", t.driver)
	}
	if t.isDefault {
		t.dbs = append(t.dbs, "")
	}
	if t.name != "" {
		t.dbs = append(t.dbs, t.name)
	}
	return t, nil
}

// conns is each migrations database ("" the default) by its nexus.toml
// block, for the tool to connect.
func conns(root string) (map[string]string, error) {
	drivers, def, err := databaseBlocks(root)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for n := range drivers {
		out[n] = n
	}
	if def != "" {
		out[""] = def
	}
	return out, nil
}
