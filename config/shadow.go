package config

import (
	"encoding"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// go-toml cannot decode a TOML string into time.Duration, and a duration is
// the one non-TOML type a config struct always wants. A section's declared
// type is therefore decoded through a "shadow" type: the same shape with
// every time.Duration replaced by a string tagged schema:"duration", which is
// converted back with time.ParseDuration. A type with no duration in it is its
// own shadow.

var (
	durationType      = reflect.TypeFor[time.Duration]()
	textUnmarshalType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// shadowType returns t's decode type and whether it differs from t.
func shadowType(t reflect.Type) (st reflect.Type, changed bool) {
	defer func() {
		// reflect.StructOf refuses some embedded shapes; those keep their
		// declared type (a duration inside then fails to decode, loudly).
		if recover() != nil {
			st, changed = t, false
		}
	}()
	return shadowOf(t)
}

func shadowOf(t reflect.Type) (reflect.Type, bool) {
	if t == durationType {
		return reflect.TypeFor[string](), true
	}
	if t.Implements(textUnmarshalType) || reflect.PointerTo(t).Implements(textUnmarshalType) {
		return t, false
	}
	switch t.Kind() {
	case reflect.Pointer:
		if e, ok := shadowOf(t.Elem()); ok {
			return reflect.PointerTo(e), true
		}
	case reflect.Slice:
		if e, ok := shadowOf(t.Elem()); ok {
			return reflect.SliceOf(e), true
		}
	case reflect.Array:
		if e, ok := shadowOf(t.Elem()); ok {
			return reflect.ArrayOf(t.Len(), e), true
		}
	case reflect.Map:
		if e, ok := shadowOf(t.Elem()); ok {
			return reflect.MapOf(t.Key(), e), true
		}
	case reflect.Struct:
		fields := make([]reflect.StructField, 0, t.NumField())
		changed := false
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			ft, ok := shadowOf(f.Type)
			if ok {
				changed = true
				if f.Type == durationType {
					f.Tag = reflect.StructTag(strings.TrimSpace(string(f.Tag) + ` schema:"duration"`))
				}
			}
			f.Type = ft
			f.Index, f.Offset = nil, 0
			fields = append(fields, f)
		}
		if changed {
			return reflect.StructOf(fields), true
		}
	}
	return t, false
}

// convertShadow copies src into dst where one is a shadow of the other.
// parse=true converts shadow → declared (strings parsed as durations);
// parse=false converts declared → shadow (durations formatted). path names
// the value in errors.
func convertShadow(dst, src reflect.Value, path []string, parse bool) error {
	if dst.Type() == src.Type() {
		dst.Set(src)
		return nil
	}
	switch {
	case parse && dst.Type() == durationType:
		s := src.String()
		if s == "" {
			dst.SetInt(0)
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("%s: %q is not a duration (write it like \"30s\" or \"1h30m\")", strings.Join(path, "."), s)
		}
		dst.SetInt(int64(d))
		return nil
	case !parse && src.Type() == durationType:
		if src.Int() == 0 {
			dst.SetString("")
		} else {
			dst.SetString(time.Duration(src.Int()).String())
		}
		return nil
	}
	switch dst.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			dst.SetZero()
			return nil
		}
		n := reflect.New(dst.Type().Elem())
		if err := convertShadow(n.Elem(), src.Elem(), path, parse); err != nil {
			return err
		}
		dst.Set(n)
	case reflect.Slice:
		if src.IsNil() {
			dst.SetZero()
			return nil
		}
		n := reflect.MakeSlice(dst.Type(), src.Len(), src.Len())
		for i := range src.Len() {
			if err := convertShadow(n.Index(i), src.Index(i), append(path, fmt.Sprint(i)), parse); err != nil {
				return err
			}
		}
		dst.Set(n)
	case reflect.Array:
		for i := range src.Len() {
			if err := convertShadow(dst.Index(i), src.Index(i), append(path, fmt.Sprint(i)), parse); err != nil {
				return err
			}
		}
	case reflect.Map:
		if src.IsNil() {
			dst.SetZero()
			return nil
		}
		n := reflect.MakeMapWithSize(dst.Type(), src.Len())
		it := src.MapRange()
		for it.Next() {
			e := reflect.New(dst.Type().Elem()).Elem()
			if err := convertShadow(e, it.Value(), append(path, fmt.Sprint(it.Key().Interface())), parse); err != nil {
				return err
			}
			n.SetMapIndex(it.Key(), e)
		}
		dst.Set(n)
	case reflect.Struct:
		for i := range dst.NumField() {
			df := dst.Type().Field(i)
			if !df.IsExported() {
				continue
			}
			sf := src.FieldByName(df.Name)
			if !sf.IsValid() {
				continue
			}
			if err := convertShadow(dst.Field(i), sf, append(path, tomlFieldName(df)), parse); err != nil {
				return err
			}
		}
	default:
		dst.Set(src.Convert(dst.Type()))
	}
	return nil
}
