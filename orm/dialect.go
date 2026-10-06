package orm

import (
	"regexp"
	"strconv"
	"strings"
)

// Dialect is what differs between the databases nexus/db opens.
type Dialect interface {
	Name() string
	// Placeholder is the n-th argument's marker, from 1.
	Placeholder(n int) string
	Quote(ident string) string
	// ILike matches col against a LIKE pattern, ignoring case.
	ILike(col, pattern string) string
	// Returning is whether INSERT … RETURNING gives back the new key.
	Returning() bool
	// NoLimit is the LIMIT to put before an OFFSET with no limit; empty
	// when OFFSET may stand alone.
	NoLimit() string
	// Violation reads a constraint error: unique or foreign key, and the
	// column it names when it names one.
	Violation(err error) (kind violation, column string)
}

type violation int

const (
	noViolation violation = iota
	uniqueViolation
	foreignKeyViolation
)

// DialectFor is the dialect of a nexus/db driver name: "postgres",
// "mysql" or "sqlite".
func DialectFor(driver string) Dialect {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx":
		return postgres{}
	case "mysql":
		return mysql{}
	}
	return sqlite{}
}

type postgres struct{}

func (postgres) Name() string                     { return "postgres" }
func (postgres) Placeholder(n int) string         { return "$" + strconv.Itoa(n) }
func (postgres) Quote(s string) string            { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (postgres) ILike(col, pattern string) string { return col + " ILIKE " + pattern + ` ESCAPE '!'` }
func (postgres) Returning() bool                  { return true }
func (postgres) NoLimit() string                  { return "" }

var (
	pgDetail     = regexp.MustCompile(`Key \(([^)]+)\)`)
	pgConstraint = regexp.MustCompile(`constraint "([^"]+)"`)
)

func (postgres) Violation(err error) (violation, string) {
	msg := err.Error()
	col := ""
	if m := pgDetail.FindStringSubmatch(msg); m != nil {
		col = m[1]
	} else if m := pgConstraint.FindStringSubmatch(msg); m != nil {
		col = m[1] // a constraint name, users_email_key: mapErr finds the column in it
	}
	switch {
	case strings.Contains(msg, "23505"), strings.Contains(msg, "duplicate key"):
		return uniqueViolation, col
	case strings.Contains(msg, "23503"), strings.Contains(msg, "foreign key"):
		return foreignKeyViolation, col
	}
	return noViolation, ""
}

type mysql struct{}

func (mysql) Name() string           { return "mysql" }
func (mysql) Placeholder(int) string { return "?" }
func (mysql) Quote(s string) string  { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }
func (mysql) ILike(col, pattern string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + pattern + ") ESCAPE '!'"
}
func (mysql) Returning() bool { return false }
func (mysql) NoLimit() string { return "18446744073709551615" }

var mysqlKey = regexp.MustCompile(`for key '(?:[^.']*\.)?([^']+)'`)

func (mysql) Violation(err error) (violation, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "1062"), strings.Contains(msg, "Duplicate entry"):
		col := ""
		if m := mysqlKey.FindStringSubmatch(msg); m != nil {
			col = m[1]
		}
		return uniqueViolation, col
	case strings.Contains(msg, "1452"), strings.Contains(msg, "1451"):
		return foreignKeyViolation, ""
	}
	return noViolation, ""
}

type sqlite struct{}

func (sqlite) Name() string           { return "sqlite" }
func (sqlite) Placeholder(int) string { return "?" }
func (sqlite) Quote(s string) string  { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (sqlite) ILike(col, pattern string) string {
	return "LOWER(" + col + ") LIKE LOWER(" + pattern + ") ESCAPE '!'"
}
func (sqlite) Returning() bool { return true }
func (sqlite) NoLimit() string { return "-1" }

var sqliteCol = regexp.MustCompile(`constraint failed: [\w"]+\.(\w+)`)

func (sqlite) Violation(err error) (violation, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		col := ""
		if m := sqliteCol.FindStringSubmatch(msg); m != nil {
			col = m[1]
		}
		return uniqueViolation, col
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return foreignKeyViolation, ""
	}
	return noViolation, ""
}

// likeEscape makes s match literally inside a LIKE pattern escaped with
// '!'.
func likeEscape(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}
