package tags

import (
	"reflect"
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
