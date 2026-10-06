package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus/orm"
)

// `nexus makemigrations` writes the next SQL migration of the ORM's models
// (Django's makemigrations): the app is built with a planner overlaid into
// its main package, run to diff the models it links against the snapshot
// the last migration left (migrations/schema.json), and the steps are
// written as migrations/NNNN_name.sql for orm.Migrate to apply at boot.

func newMakeMigrationsCmd(stdout, stderr io.Writer) *cobra.Command {
	var o makeMigrationsOpts
	cmd := &cobra.Command{
		Use:   "makemigrations [name]",
		Short: "Write the next SQL migration of the ORM's models",
		Long: `Write the next SQL migration of the ORM's models.

The app is built and run as a planner (its main never runs) to compare the
models it declares with orm.For against the snapshot the last migration left
(<dir>/schema.json). The difference is written as <dir>/NNNN_<name>.sql and
the snapshot updated; orm.Migrate applies the files at boot.

The dialect is the driver of the database's [databases.<db>] block in
nexus.toml (--dialect overrides). Read the file before committing it: a
column rename shows as a drop and an add, and steps that need a person are
marked "-- NOTE:".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.name = args[0]
			}
			return runMakeMigrations(o, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&o.root, "root", ".", "project directory")
	cmd.Flags().StringVar(&o.db, "db", "", "the [databases.<db>] block (default: the default database)")
	cmd.Flags().StringVar(&o.dialect, "dialect", "", "postgres | mysql | sqlite (default: the block's driver)")
	cmd.Flags().StringVar(&o.dir, "dir", "", "migrations directory (default migrations, migrations/<db> for a database that isn't the default)")
	cmd.Flags().BoolVar(&o.check, "check", false, "fail when the models have changes no migration holds, writing nothing (CI)")
	cmd.Flags().BoolVar(&o.empty, "empty", false, "write an empty migration for hand-written SQL")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the migration instead of writing it")
	return cmd
}

type makeMigrationsOpts struct {
	root, db, dialect, dir, name string
	check, empty, dryRun         bool
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
		dir = "migrations"
		if !target.isDefault {
			dir = filepath.Join("migrations", target.name)
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if o.name != "" && !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(o.name) {
		return fmt.Errorf("a migration name is letters, digits and _: %q", o.name)
	}
	next, err := nextMigration(dir)
	if err != nil {
		return err
	}
	if o.empty {
		name := o.name
		if name == "" {
			name = "custom"
		}
		return writeMigration(stdout, filepath.Join(dir, fmt.Sprintf("%04d_%s.sql", next, name)), []byte("-- Write the migration's SQL here: statements end with a semicolon.\n"), o.dryRun)
	}

	snapshot := filepath.Join(dir, "schema.json")
	plan, err := runPlanner(root, orm.PlanRequest{Snapshot: snapshot, Driver: target.driver, DBs: target.dbs}, stderr)
	if err != nil {
		return err
	}
	if len(plan.Steps) == 0 {
		fmt.Fprintf(stdout, "No changes in the models of %s.\n", target.label())
		return nil
	}
	if o.check {
		fmt.Fprintf(stderr, "The models of %s have changes no migration holds:\n", target.label())
		for _, s := range plan.Steps {
			if s.SQL != "" {
				fmt.Fprintf(stderr, "  %s\n", firstLine(s.SQL))
			}
		}
		return errors.New("run nexus makemigrations")
	}
	name := o.name
	if name == "" {
		name = migrationAutoName(plan.Steps, next == 1)
	}
	path := filepath.Join(dir, fmt.Sprintf("%04d_%s.sql", next, name))
	if err := writeMigration(stdout, path, orm.MigrationFile(plan.Steps), o.dryRun); err != nil {
		return err
	}
	for _, s := range plan.Steps {
		if s.Note != "" {
			fmt.Fprintf(stderr, "  ! %s\n", s.Note)
		}
	}
	if o.dryRun {
		return nil
	}
	return os.WriteFile(snapshot, append(plan.Snapshot, '\n'), 0o644)
}

func writeMigration(stdout io.Writer, path string, content []byte, dryRun bool) error {
	if dryRun {
		fmt.Fprintf(stdout, "-- %s\n%s", path, content)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", path)
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

var migrationFileRE = regexp.MustCompile(`^(\d+)_[A-Za-z0-9_]+\.sql$`)

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

var stepTableRE = regexp.MustCompile(`^(?:CREATE TABLE|ALTER TABLE|DROP TABLE)\s+["` + "`" + `]?([A-Za-z0-9_]+)`)

// migrationAutoName names a migration after what it does: initial, the
// one table it touches, else auto.
func migrationAutoName(steps []orm.MigrationStep, first bool) string {
	if first {
		return "initial"
	}
	tables := map[string]bool{}
	for _, s := range steps {
		if m := stepTableRE.FindStringSubmatch(s.SQL); m != nil {
			tables[m[1]] = true
		}
	}
	if len(tables) == 1 {
		for t := range tables {
			return t
		}
	}
	return "auto"
}

// migrationTarget is the database a migration is for: its nexus.toml
// name, driver, and the orm.On names of its models.
type migrationTargetInfo struct {
	name, driver string
	isDefault    bool
	dbs          []string
}

func (t migrationTargetInfo) label() string {
	if t.name == "" {
		return "the default database"
	}
	return "database " + t.name
}

func migrationTarget(root, db, dialect string) (migrationTargetInfo, error) {
	var doc struct {
		Databases map[string]struct {
			Driver  string `toml:"driver"`
			Default bool   `toml:"default"`
		} `toml:"databases"`
	}
	if raw, err := os.ReadFile(filepath.Join(root, "nexus.toml")); err == nil {
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return migrationTargetInfo{}, fmt.Errorf("nexus.toml: %w", err)
		}
	}
	names := make([]string, 0, len(doc.Databases))
	for n := range doc.Databases {
		names = append(names, n)
	}
	sort.Strings(names)
	t := migrationTargetInfo{name: db}
	if db == "" {
		for _, n := range names {
			if doc.Databases[n].Default {
				t.name = n
			}
		}
		if t.name == "" && len(names) == 1 {
			t.name = names[0]
		}
		t.isDefault = true
	} else if _, ok := doc.Databases[db]; !ok && dialect == "" {
		return t, fmt.Errorf("nexus.toml has no [databases.%s] (or pass --dialect)", db)
	} else {
		d := doc.Databases[db]
		t.isDefault = d.Default || len(names) == 1
	}
	t.driver = dialect
	if t.driver == "" && t.name != "" {
		t.driver = doc.Databases[t.name].Driver
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

// planFileName is the planner's file in the main package: an init that,
// with orm.PlanEnv set, writes the plan and exits before main. Named to
// sort first, so its init runs before the app's own.
const planFileName = "0_nexus_orm_plan.go"

const planFile = `// Code generated by "nexus makemigrations"; DO NOT EDIT.

package main

import nexusorm "github.com/paulmanoni/nexus/orm"

func init() { nexusorm.ServePlan() }
`

// runPlanner builds the app with the planner overlaid (and the generated
// files nexus build overlays) and runs it for the plan.
func runPlanner(root string, req orm.PlanRequest, stderr io.Writer) (orm.MigrationPlan, error) {
	var plan orm.MigrationPlan
	mainDir, err := pkgs.mainDir(root)
	if err != nil {
		return plan, err
	}
	if mainDir == "" {
		return plan, errors.New("no main package: the planner runs the app, so makemigrations needs one")
	}
	tmp, err := os.MkdirTemp("", "nexus-makemigrations-")
	if err != nil {
		return plan, err
	}
	defer os.RemoveAll(tmp)

	overlay, cleanup, err := buildOverlay(root, "", true, false)
	if err != nil {
		return plan, err
	}
	defer cleanup()
	doc := struct{ Replace map[string]string }{Replace: map[string]string{}}
	if overlay != "" {
		raw, err := os.ReadFile(overlay)
		if err != nil {
			return plan, err
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return plan, err
		}
	}
	shadow := filepath.Join(tmp, planFileName)
	if err := os.WriteFile(shadow, []byte(planFile), 0o644); err != nil {
		return plan, err
	}
	doc.Replace[filepath.Join(mainDir, planFileName)] = shadow
	overlayPath := filepath.Join(tmp, "overlay.json")
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(overlayPath, raw, 0o644); err != nil {
		return plan, err
	}

	bin := filepath.Join(tmp, "planner")
	build := exec.Command("go", "build", "-overlay", overlayPath, "-o", bin, ".")
	build.Dir = mainDir
	build.Stdout, build.Stderr = stderr, stderr
	if err := build.Run(); err != nil {
		return plan, fmt.Errorf("building the app as the planner: %w", err)
	}
	req.Out = filepath.Join(tmp, "plan.json")
	reqJSON, _ := json.Marshal(req)
	run := exec.Command(bin)
	run.Dir = root
	run.Env = append(os.Environ(), orm.PlanEnv+"="+string(reqJSON))
	run.Stdout, run.Stderr = stderr, stderr
	if err := run.Run(); err != nil {
		return plan, fmt.Errorf("running the planner: %w", err)
	}
	out, err := os.ReadFile(req.Out)
	if err != nil {
		return plan, fmt.Errorf("the planner wrote no plan (does the app's main package link the orm?): %w", err)
	}
	return plan, json.Unmarshal(out, &plan)
}
