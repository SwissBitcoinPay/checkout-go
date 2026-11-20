// Package testvec reads the conformance vectors of the pinned specification (spec/vectors) for
// the tests: JSON read as every implementation reads it (jsonv), lone surrogates kept.
package testvec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/sattleme/checkout-go/internal/jsonv"
)

// Root is the repository's root directory.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Path is the path of a file of spec/vectors.
func Path(name string) string {
	return filepath.Join(Root(), "spec", "vectors", filepath.FromSlash(name))
}

// Load reads a vector file of spec/vectors.
func Load(tb testing.TB, name string) *jsonv.Object {
	tb.Helper()
	data, err := os.ReadFile(Path(name))
	if err != nil {
		tb.Fatal(err)
	}
	v, err := jsonv.Parse(data)
	if err != nil {
		tb.Fatalf("%s: %v", name, err)
	}
	o, ok := jsonv.AsObject(v)
	if !ok {
		tb.Fatalf("%s: not an object", name)
	}
	return o
}

// Obj returns the object member name of o (fatal when it is not one).
func Obj(tb testing.TB, o *jsonv.Object, name string) *jsonv.Object {
	tb.Helper()
	v, _ := o.Get(name)
	r, ok := jsonv.AsObject(v)
	if !ok {
		tb.Fatalf("%q is not an object", name)
	}
	return r
}

// Arr returns the array member name of o (fatal when it is not one).
func Arr(tb testing.TB, o *jsonv.Object, name string) []any {
	tb.Helper()
	v, _ := o.Get(name)
	r, ok := jsonv.AsArray(v)
	if !ok {
		tb.Fatalf("%q is not an array", name)
	}
	return r
}

// Objs returns the array member name of o as objects (fatal when it is not one).
func Objs(tb testing.TB, o *jsonv.Object, name string) []*jsonv.Object {
	tb.Helper()
	items := Arr(tb, o, name)
	r := make([]*jsonv.Object, len(items))
	for i, item := range items {
		obj, ok := jsonv.AsObject(item)
		if !ok {
			tb.Fatalf("%q[%d] is not an object", name, i)
		}
		r[i] = obj
	}
	return r
}

// Str returns the string member name of o (fatal when it is not one).
func Str(tb testing.TB, o *jsonv.Object, name string) string {
	tb.Helper()
	v, _ := o.Get(name)
	s, ok := v.(string)
	if !ok {
		tb.Fatalf("%q is not a string", name)
	}
	return s
}

// Num returns the number member name of o (fatal when it is not one).
func Num(tb testing.TB, o *jsonv.Object, name string) float64 {
	tb.Helper()
	v, _ := o.Get(name)
	f, ok := v.(float64)
	if !ok {
		tb.Fatalf("%q is not a number", name)
	}
	return f
}

// Int returns the integer member name of o (fatal when it is not a safe integer).
func Int(tb testing.TB, o *jsonv.Object, name string) int64 {
	tb.Helper()
	v, _ := o.Get(name)
	n, ok := jsonv.AsSafeInteger(v)
	if !ok {
		tb.Fatalf("%q is not an integer", name)
	}
	return n
}

// Get returns the member name of o (nil when absent).
func Get(o *jsonv.Object, name string) any {
	v, _ := o.Get(name)
	return v
}

// Canon is the JSON of v (anything jsonv writes) with the members of every object sorted by name:
// two values are equal as JSON, whatever the order of their members, when their Canon is.
func Canon(v any) string {
	parsed, err := jsonv.Parse(encode(v))
	if err != nil {
		panic(err)
	}
	return string(jsonv.Marshal(sorted(parsed)))
}

// encode writes v as jsonv does, or, for a type jsonv does not write (a map, a struct with its
// own MarshalJSON), as encoding/json does.
func encode(v any) (data []byte) {
	defer func() {
		if recover() != nil {
			var err error
			if data, err = json.Marshal(v); err != nil {
				panic(err)
			}
		}
	}()
	return jsonv.Marshal(v)
}

func sorted(v any) any {
	switch v := v.(type) {
	case *jsonv.Object:
		keys := append([]string(nil), v.Keys()...)
		sort.Strings(keys)
		m := make(jsonv.Members, len(keys))
		for i, k := range keys {
			value, _ := v.Get(k)
			m[i] = jsonv.Member{Name: k, Value: sorted(value)}
		}
		return m
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sorted(item)
		}
		return out
	}
	return v
}

// Equal fails the test unless got and want are equal as JSON (Canon), whatever the order of their
// objects' members.
func Equal(tb testing.TB, name string, got, want any) {
	tb.Helper()
	if g, w := Canon(got), Canon(want); g != w {
		tb.Errorf("%s:\ngot  %s\nwant %s", name, g, w)
	}
}
