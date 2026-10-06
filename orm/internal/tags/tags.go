package tags

import (
	"reflect"
	"strconv"
	"strings"
	"unicode"
)

// Tags is a field's column settings.
type Tags struct {
	Column              string
	Skip, PK            bool
	AutoNowAdd, AutoNow bool
	Embedded, Computed  bool
	Prefix              string

	// A relation: FK names the column holding the related row's key
	// (orm:"fk:author_id", GORM's foreignKey on a belongs-to), Rel the
	// related rows' column holding this row's key (orm:"rel:author_id",
	// GORM's foreignKey on a has-many), M2M the table between
	// (orm:"m2m:post_tags[,post_id,tag_id]", GORM's many2many).
	FK, Rel, M2M string

	// Schema: a UNIQUE column, a VARCHAR size, a database type of your
	// own, an index, and what deleting the related row does to this one
	// (on a foreign key: cascade, set_null, restrict).
	Unique, Index bool
	Size          int
	Type          string
	OnDelete      string
	// GormForeignKey is GORM's foreignKey, a Go field name, read as FK or
	// Rel by the field's shape.
	GormForeignKey string
}

// Parse reads a field's column settings from its name, tag and whether
// it is a time.Time: orm first, then db, then GORM's, so models written
// for GORM need no new tags. The ORM and its generator share it, so they
// agree on the columns.
func Parse(name string, tag reflect.StructTag, isTime bool) Tags {
	var t Tags
	if v, ok := tag.Lookup("orm"); ok {
		if v == "-" {
			t.Skip = true
			return t
		}
		for part := range strings.SplitSeq(v, ";") {
			k, val, _ := strings.Cut(strings.TrimSpace(part), ":")
			switch strings.ToLower(k) {
			case "column":
				t.Column = val
			case "pk":
				t.PK = true
			case "auto_now_add":
				t.AutoNowAdd = true
			case "auto_now":
				t.AutoNow = true
			case "embedded":
				t.Embedded = true
			case "computed":
				t.Computed = true
			case "unique":
				t.Unique = true
			case "index":
				t.Index = true
			case "size":
				t.Size, _ = strconv.Atoi(val)
			case "type":
				t.Type = val
			case "on_delete":
				t.OnDelete = strings.ToLower(val)
			case "fk":
				t.FK = val
			case "rel":
				t.Rel = val
			case "m2m":
				t.M2M = val
			case "prefix":
				t.Prefix = val
			}
		}
	}
	if v, ok := tag.Lookup("db"); ok && t.Column == "" {
		if v == "-" {
			t.Skip = true
			return t
		}
		t.Column, _, _ = strings.Cut(v, ",")
	}
	if v, ok := tag.Lookup("gorm"); ok {
		for part := range strings.SplitSeq(v, ";") {
			k, val, _ := strings.Cut(strings.TrimSpace(part), ":")
			switch strings.ToLower(k) {
			case "-":
				t.Skip = true
			case "column":
				if t.Column == "" {
					t.Column = val
				}
			case "primarykey", "primary_key":
				t.PK = true
			case "autocreatetime":
				t.AutoNowAdd = true
			case "autoupdatetime":
				t.AutoNow = true
			case "embedded":
				t.Embedded = true
			case "foreignkey":
				t.GormForeignKey = val
			case "unique", "uniqueindex":
				t.Unique = true
			case "index":
				t.Index = true
			case "size":
				if t.Size == 0 {
					t.Size, _ = strconv.Atoi(val)
				}
			case "type":
				if t.Type == "" {
					t.Type = val
				}
			case "constraint":
				if v := strings.ToLower(val); strings.Contains(v, "ondelete:") {
					t.OnDelete = strings.ReplaceAll(strings.TrimSpace(strings.SplitN(strings.SplitN(v, "ondelete:", 2)[1], ",", 2)[0]), " ", "_")
				}
			case "many2many":
				if t.M2M == "" {
					t.M2M = val
				}
			case "embeddedprefix":
				t.Prefix = val
			}
		}
	}
	if !t.AutoNowAdd && !t.AutoNow && isTime {
		switch name {
		case "CreatedAt":
			t.AutoNowAdd = true
		case "UpdatedAt":
			t.AutoNow = true
		}
	}
	return t
}

// Snake is a Go name as a column: UserID → user_id, CreatedAt → created_at.
func Snake(s string) string {
	var b strings.Builder
	r := []rune(s)
	for i, c := range r {
		if unicode.IsUpper(c) {
			if i > 0 && (unicode.IsLower(r[i-1]) || (i+1 < len(r) && unicode.IsLower(r[i+1]) && unicode.IsUpper(r[i-1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(c))
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Plural is a table name from a model's: user → users, category →
// categories, address → addresses.
func Plural(s string) string {
	switch {
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}
