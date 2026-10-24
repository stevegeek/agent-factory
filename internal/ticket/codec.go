package ticket

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// The codec between the ordered tree (jsonx) and the typed views. It is
// driven by struct tags, `json:"key"` with the options:
//
//   - omitempty: a zero value is not written into a new object. (A plain
//     field is written into a new object even when zero, because Python
//     writes "draft": false and "blockers": [].)
//   - "-": not stored.
//
// Field order is the order Python inserts the keys, which is the order a
// new key is appended in; a struct with a keyOrder method chooses it per
// value. A nil slice is absent; an empty one is [].
//
// Decoding is lenient: a value of the wrong type reads as the zero value and
// is reported as a Problem. Encoding merges the views back into the tree
// they were read from:
//
//   - a value whose typed view did not change keeps its stored bytes, which
//     also keeps wrong-typed values, null-for-zero, number literals and
//     string escapes;
//   - an object keeps its keys in place, appends new ones in field order,
//     drops a key whose Opt is unset (Python's pop), and keeps every key no
//     field names, at every depth;
//   - an array merges element by element, cut or extended to the new length.
//
// A plain (non-Opt) field whose key is absent from an existing object is
// written only when it is not zero, so reading and saving a ticket never adds
// keys to it.

// Problem is a stored value a typed view could not read.
type Problem struct {
	Path string // "pr.number", "history[3].note"
	Want string // the Go type the view expected
	Have string // the JSON kind found
}

func (p Problem) String() string { return fmt.Sprintf("%s: want %s, have %s", p.Path, p.Want, p.Have) }

// keyOrderer chooses the order in which a struct's new keys are appended.
type keyOrderer interface{ keyOrder() []string }

// jsonDecoder and jsonMerger let a type (Entry) take over its own codec.
type jsonDecoder interface {
	decodeJSON(d *decoder, path string, v jsonx.Value) bool
}

type jsonMerger interface {
	mergeJSON(old jsonx.Object, hasOld bool) jsonx.Value
}

type field struct {
	index     int
	key       string
	omitEmpty bool
}

func fieldsOf(t reflect.Type) []field {
	var out []field
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		out = append(out, field{index: i, key: name, omitEmpty: opts == "omitempty"})
	}
	return out
}

// orderedFields are rv's fields in the order new keys are appended.
func orderedFields(rv reflect.Value) []field {
	fs := fieldsOf(rv.Type())
	ko, ok := rv.Interface().(keyOrderer)
	if !ok {
		return fs
	}
	byKey := map[string]field{}
	for _, f := range fs {
		byKey[f.key] = f
	}
	var out []field
	for _, k := range ko.keyOrder() {
		if f, ok := byKey[k]; ok {
			out = append(out, f)
			delete(byKey, k)
		}
	}
	for _, f := range fs {
		if _, left := byKey[f.key]; left {
			out = append(out, f)
		}
	}
	return out
}

func isOpt(t reflect.Type) bool { return t.Implements(reflect.TypeFor[optional]()) }

type decoder struct{ problems []Problem }

func (d *decoder) bad(path string, t reflect.Type, v jsonx.Value) {
	d.problems = append(d.problems, Problem{Path: strings.TrimPrefix(path, "."), Want: typeName(t), Have: v.Kind().String()})
}

func typeName(t reflect.Type) string {
	switch t {
	case reflect.TypeFor[jsonx.Object]():
		return "object"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64:
		return "int"
	case reflect.Float64:
		return "number"
	case reflect.Slice:
		return "array"
	case reflect.Struct:
		return "object"
	}
	return t.String()
}

// decode reads v into rv (settable, holding its zero value) and reports
// whether v had the right type. A null reads as the zero value.
func (d *decoder) decode(path string, v jsonx.Value, rv reflect.Value) bool {
	t := rv.Type()
	if pd, ok := rv.Addr().Interface().(jsonDecoder); ok {
		return pd.decodeJSON(d, path, v)
	}
	switch t {
	case reflect.TypeFor[jsonx.Value]():
		rv.Set(reflect.ValueOf(v.Clone()))
		return true
	case reflect.TypeFor[jsonx.Object]():
		if v.IsNull() {
			return true
		}
		o, ok := v.Object()
		if !ok {
			d.bad(path, t, v)
			return false
		}
		rv.Set(reflect.ValueOf(o.Clone()))
		return true
	}
	if isOpt(t) {
		if v.IsNull() {
			rv.FieldByName("Set").SetBool(true)
			rv.FieldByName("Null").SetBool(true)
			return true
		}
		if !d.decode(path, v, rv.FieldByName("V")) {
			rv.FieldByName("V").SetZero()
			return false
		}
		rv.FieldByName("Set").SetBool(true)
		return true
	}
	if v.IsNull() {
		return true
	}
	switch t.Kind() {
	case reflect.String:
		if s, ok := v.Str(); ok {
			rv.SetString(s)
			return true
		}
	case reflect.Bool:
		if b, ok := v.Bool(); ok {
			rv.SetBool(b)
			return true
		}
	case reflect.Int, reflect.Int64:
		if n, ok := v.Int64(); ok {
			rv.SetInt(n)
			return true
		}
	case reflect.Float64:
		if f, ok := v.Float64(); ok {
			rv.SetFloat(f)
			return true
		}
	case reflect.Slice:
		if a, ok := v.Array(); ok {
			s := reflect.MakeSlice(t, len(a), len(a))
			for i, e := range a {
				d.decode(path+"["+strconv.Itoa(i)+"]", e, s.Index(i))
			}
			rv.Set(s)
			return true
		}
	case reflect.Struct:
		if o, ok := v.Object(); ok {
			d.decodeFields(path, o, rv)
			return true
		}
	}
	d.bad(path, t, v)
	return false
}

func (d *decoder) decodeFields(path string, o jsonx.Object, rv reflect.Value) {
	for _, f := range fieldsOf(rv.Type()) {
		if v, ok := o.Lookup(f.key); ok {
			d.decode(path+"."+f.key, v, rv.Field(f.index))
		}
	}
}

// unchanged reports that rv is what decoding old gives, so old's bytes stand.
func unchanged(old jsonx.Value, rv reflect.Value) bool {
	probe := reflect.New(rv.Type()).Elem()
	(&decoder{}).decode("", old, probe)
	return reflect.DeepEqual(probe.Interface(), rv.Interface())
}

// decodesClean reports that old has the kind a t is read from (an object for
// a struct, an array for a slice...), whatever problems lie deeper.
func decodesClean(old jsonx.Value, t reflect.Type) bool {
	return (&decoder{}).decode("", old, reflect.New(t).Elem())
}

// merge writes the view rv back over old (when hasOld) and returns the value
// to store.
func merge(old jsonx.Value, hasOld bool, rv reflect.Value) jsonx.Value {
	if hasOld && unchanged(old, rv) {
		return old
	}
	t := rv.Type()
	if m, ok := rv.Interface().(jsonMerger); ok {
		o, isObj := old.Object()
		return m.mergeJSON(o, hasOld && isObj)
	}
	switch t {
	case reflect.TypeFor[jsonx.Value]():
		cur := rv.Interface().(jsonx.Value)
		if hasOld && jsonx.Equal(old, cur) {
			return old
		}
		return cur
	case reflect.TypeFor[jsonx.Object]():
		cur := rv.Interface().(jsonx.Object)
		o, _ := old.Object()
		out := make(jsonx.Object, len(cur))
		for i, m := range cur {
			out[i] = m
			if ov, ok := o.Lookup(m.Key); ok && jsonx.Equal(ov, m.Value) {
				out[i].Value = ov
			}
		}
		return out.Value()
	}
	if isOpt(t) {
		if rv.FieldByName("Null").Bool() {
			return jsonx.Null()
		}
		// a stored value the view could not read is replaced, not merged into
		v := rv.FieldByName("V")
		return merge(old, hasOld && decodesClean(old, v.Type()), v)
	}
	switch t.Kind() {
	case reflect.String:
		return jsonx.String(rv.String())
	case reflect.Bool:
		return jsonx.Bool(rv.Bool())
	case reflect.Int, reflect.Int64:
		return jsonx.Int(rv.Int())
	case reflect.Float64:
		return jsonx.Float(rv.Float())
	case reflect.Slice:
		var oldArr []jsonx.Value
		if hasOld {
			oldArr, _ = old.Array()
		}
		out := make([]jsonx.Value, rv.Len())
		for i := range out {
			if i < len(oldArr) {
				out[i] = merge(oldArr[i], true, rv.Index(i))
			} else {
				out[i] = merge(jsonx.Null(), false, rv.Index(i))
			}
		}
		return jsonx.ArrayOf(out...)
	case reflect.Struct:
		o, isObj := old.Object()
		return mergeFields(o, hasOld && isObj, rv).Value()
	}
	panic(fmt.Sprintf("ticket: no codec for %s", t))
}

// mergeFields merges rv's fields into a copy of old (an existing object when
// hasOld, else a new one).
func mergeFields(old jsonx.Object, hasOld bool, rv reflect.Value) jsonx.Object {
	out := make(jsonx.Object, len(old))
	copy(out, old)
	for _, f := range orderedFields(rv) {
		cur := rv.Field(f.index)
		i := out.Index(f.key)
		var write bool
		switch {
		case isOpt(cur.Type()):
			// unset: Python popped it, unless the stored value is one the
			// view could not read and nobody changed
			write = cur.FieldByName("Set").Bool() || i >= 0 && unchanged(out[i].Value, cur)
		case i >= 0:
			write = true
		case hasOld:
			write = !cur.IsZero()
		default:
			write = !(f.omitEmpty && cur.IsZero())
		}
		switch {
		case !write && i >= 0:
			out.Delete(f.key)
		case !write:
		case i >= 0:
			out[i].Value = merge(out[i].Value, true, cur)
		default:
			out = append(out, jsonx.Member{Key: f.key, Value: merge(jsonx.Null(), false, cur)})
		}
	}
	return out
}
