package yamlite

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var (
	intRe   = regexp.MustCompile(`^[-+]?[0-9]+$`)
	floatRe = regexp.MustCompile(`^[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?$`)
)

// Unmarshal decodes YAML into v, which must be a non-nil pointer. Struct
// fields are matched by their json tag names; unknown keys are an error.
func Unmarshal(data []byte, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("yamlite: Unmarshal needs a non-nil pointer")
	}
	root, err := parse(data)
	if err != nil {
		return err
	}
	return decode(root, rv.Elem(), "")
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func at(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

func decode(n *node, v reflect.Value, path string) error {
	if v.Kind() == reflect.Pointer {
		if n.kind == nullNode {
			v.Set(reflect.Zero(v.Type()))
			return nil
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return decode(n, v.Elem(), path)
	}
	if v.Kind() == reflect.Interface && v.NumMethod() == 0 {
		g := generic(n)
		if g == nil {
			v.Set(reflect.Zero(v.Type()))
		} else {
			v.Set(reflect.ValueOf(g))
		}
		return nil
	}
	if n.kind == nullNode {
		v.Set(reflect.Zero(v.Type()))
		return nil
	}
	mismatch := func(want string) error {
		return errAt(n.line, "%s: expected %s, found %s", at(path), want, n.describe())
	}
	switch v.Kind() {
	case reflect.Struct:
		if n.kind != mapNode {
			return mismatch("a mapping")
		}
		fields := structFields(v.Type())
		for i, key := range n.keys {
			idx, ok := fields[key]
			if !ok {
				return errAt(n.vals[i].line, "%s: unknown field %q", at(path), key)
			}
			if err := decode(n.vals[i], v.Field(idx), join(path, key)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("yamlite: %s: only string-keyed maps are supported", at(path))
		}
		if n.kind != mapNode {
			return mismatch("a mapping")
		}
		m := reflect.MakeMapWithSize(v.Type(), len(n.keys))
		for i, key := range n.keys {
			elem := reflect.New(v.Type().Elem()).Elem()
			if err := decode(n.vals[i], elem, join(path, key)); err != nil {
				return err
			}
			m.SetMapIndex(reflect.ValueOf(key).Convert(v.Type().Key()), elem)
		}
		v.Set(m)
		return nil
	case reflect.Slice:
		if n.kind != seqNode {
			return mismatch("a sequence")
		}
		s := reflect.MakeSlice(v.Type(), len(n.items), len(n.items))
		for i, item := range n.items {
			if err := decode(item, s.Index(i), fmt.Sprintf("%s[%d]", at(path), i)); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	}

	if n.kind != scalarNode {
		return mismatch("a single value")
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(n.text)
		return nil
	case reflect.Bool:
		if !n.quoted {
			switch n.text {
			case "true", "True", "TRUE":
				v.SetBool(true)
				return nil
			case "false", "False", "FALSE":
				v.SetBool(false)
				return nil
			}
		}
		return mismatch("true or false")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n.quoted || !intRe.MatchString(n.text) {
			return mismatch("a whole number")
		}
		i, err := strconv.ParseInt(strings.TrimPrefix(n.text, "+"), 10, v.Type().Bits())
		if err != nil {
			return errAt(n.line, "%s: %q is out of range", at(path), n.text)
		}
		v.SetInt(i)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n.quoted || !intRe.MatchString(n.text) || strings.HasPrefix(n.text, "-") {
			return mismatch("a non-negative whole number")
		}
		u, err := strconv.ParseUint(strings.TrimPrefix(n.text, "+"), 10, v.Type().Bits())
		if err != nil {
			return errAt(n.line, "%s: %q is out of range", at(path), n.text)
		}
		v.SetUint(u)
		return nil
	case reflect.Float32, reflect.Float64:
		if n.quoted || !floatRe.MatchString(n.text) {
			return mismatch("a number")
		}
		f, err := strconv.ParseFloat(n.text, v.Type().Bits())
		if err != nil {
			return errAt(n.line, "%s: %q is not a valid number", at(path), n.text)
		}
		v.SetFloat(f)
		return nil
	}
	return fmt.Errorf("yamlite: %s: unsupported Go type %s", at(path), v.Type())
}

// structFields maps json tag names (or field names) to field indexes.
func structFields(t reflect.Type) map[string]int {
	out := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			tagName, _, _ := strings.Cut(tag, ",")
			if tagName == "-" {
				continue
			}
			if tagName != "" {
				name = tagName
			}
		}
		out[name] = i
	}
	return out
}

// generic converts a node into map[string]any, []any, string, bool,
// int64, float64 or nil, following the YAML 1.2 core schema for plain
// scalars.
func generic(n *node) any {
	switch n.kind {
	case nullNode:
		return nil
	case mapNode:
		m := make(map[string]any, len(n.keys))
		for i, k := range n.keys {
			m[k] = generic(n.vals[i])
		}
		return m
	case seqNode:
		s := make([]any, len(n.items))
		for i, item := range n.items {
			s[i] = generic(item)
		}
		return s
	}
	if n.quoted {
		return n.text
	}
	switch n.text {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	}
	if intRe.MatchString(n.text) {
		if i, err := strconv.ParseInt(strings.TrimPrefix(n.text, "+"), 10, 64); err == nil {
			return i
		}
	}
	if floatRe.MatchString(n.text) {
		if f, err := strconv.ParseFloat(n.text, 64); err == nil {
			return f
		}
	}
	return n.text
}
