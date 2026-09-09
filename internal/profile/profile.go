// Package profile turns a sample of documents (or rows) into the field
// list the mapping screen shows: for every path, how many sampled records
// held each type, a few short example values, and whether the values were
// all distinct. Both engines feed it plain Go values (map[string]any,
// []any, string, int64, float64, bool, time.Time, []byte, nil), so the
// walk, the JSON-string detection and the example rules live in one place.
//
// What leaves the machine from here is deliberately small: type counts and
// at most `examples` values per field, each cut to 40 characters; long
// text, binary and containers give no examples at all.
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/5panel/agent/internal/protocol"
)

// Defaults and caps of the walk.
const (
	DefaultMaxDepth = 3       // "Nested paths are listed to a depth of 3."
	ExampleLen      = 40      // characters kept of a string example
	LongText        = 256     // strings longer than this give no example
	DefaultMaxField = 500     // fields kept per profile; keeps the answer bounded
	MaxJSONText     = 1 << 20 // strings longer than this are not tried as JSON
)

// Options tune a Profiler.
type Options struct {
	Examples  int // examples kept per field (0 = none)
	MaxDepth  int // dotted depth, default 3
	MaxFields int // default 500
}

// Profiler accumulates documents; call Add for each, then Fields.
type Profiler struct {
	opts   Options
	docs   int
	fields []*field
	index  map[string]*field
}

type field struct {
	path     string
	types    map[string]int
	examples []any
	exampleK map[string]bool
	distinct map[string]bool
	nonNull  int
	unique   bool // stays true only while every value was distinct and non-null
}

// New returns a Profiler with defaults filled in.
func New(opts Options) *Profiler {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.MaxFields <= 0 {
		opts.MaxFields = DefaultMaxField
	}
	if opts.Examples < 0 {
		opts.Examples = 0
	}
	return &Profiler{opts: opts, index: map[string]*field{}}
}

// Build profiles docs in one call.
func Build(docs []map[string]any, opts Options) []protocol.FieldProfile {
	p := New(opts)
	for _, d := range docs {
		p.Add(d)
	}
	return p.Fields()
}

// Add walks one document.
func (p *Profiler) Add(doc map[string]any) {
	p.docs++
	p.walkObject("", 1, doc)
}

// Docs is the number of documents added.
func (p *Profiler) Docs() int { return p.docs }

func (p *Profiler) field(path string) *field {
	if f, ok := p.index[path]; ok {
		return f
	}
	if len(p.fields) >= p.opts.MaxFields {
		return nil
	}
	f := &field{path: path, types: map[string]int{}, examples: []any{}, exampleK: map[string]bool{}, distinct: map[string]bool{}, unique: true}
	p.fields = append(p.fields, f)
	p.index[path] = f
	return f
}

func (p *Profiler) walkObject(prefix string, depth int, obj map[string]any) {
	if depth > p.opts.MaxDepth {
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		// Keys that are not path segments (numeric slot keys like "1",
		// keys with dots) cannot be requested, so they are not listed.
		if protocol.IsSegment(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		p.walk(path, depth, obj[k])
	}
}

func (p *Profiler) walk(path string, depth int, v any) {
	f := p.field(path)
	if f == nil {
		return
	}
	switch x := v.(type) {
	case nil:
		f.types["null"]++
		f.unique = false
	case string:
		if inner, ok := ParseJSONText(x); ok {
			f.types["json"]++
			f.unique = false
			p.walkChildren(path, depth, inner)
			return
		}
		f.types["string"]++
		if len(x) > LongText {
			f.value("s:"+x, nil, p.opts.Examples)
		} else {
			f.value("s:"+x, truncate(x), p.opts.Examples)
		}
	case bool:
		f.types["boolean"]++
		f.value(fmt.Sprintf("b:%v", x), x, p.opts.Examples)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		f.types["number"]++
		f.value(fmt.Sprintf("n:%v", x), x, p.opts.Examples)
	case json.Number:
		f.types["number"]++
		f.value("n:"+x.String(), protocol.NormalizeNumbers(x), p.opts.Examples)
	case time.Time:
		f.types["date"]++
		s := x.UTC().Format(time.RFC3339Nano)
		f.value("d:"+s, s, p.opts.Examples)
	case []byte:
		f.types["binary"]++
		f.unique = false
	case map[string]any:
		f.types["json"]++
		f.unique = false
		p.walkChildren(path, depth, x)
	case []any:
		f.types["json"]++
		f.unique = false
		p.walkChildren(path, depth, x)
	default:
		f.types["other"]++
		f.unique = false
	}
}

// walkChildren descends into a container: object keys at depth+1, array
// elements under "path[]" at the same depth. An array inside an array is
// described as json at "path[]" and not opened further, since a path can
// carry only one "[]" per segment.
func (p *Profiler) walkChildren(path string, depth int, v any) {
	switch x := v.(type) {
	case map[string]any:
		p.walkObject(path, depth+1, x)
	case []any:
		if strings.HasSuffix(path, "[]") {
			return
		}
		for _, el := range x {
			p.walk(path+"[]", depth, el)
		}
	}
}

// value records a non-null scalar: tracks distinctness and keeps the
// example when there is room. example nil means "no example for this one".
func (f *field) value(key string, example any, maxExamples int) {
	f.nonNull++
	if f.unique {
		if f.distinct[key] {
			f.unique = false
			f.distinct = nil
		} else {
			f.distinct[key] = true
		}
	}
	if example == nil || len(f.examples) >= maxExamples || f.exampleK[key] {
		return
	}
	f.exampleK[key] = true
	f.examples = append(f.examples, example)
}

// Fields returns the profile in first-seen order.
func (p *Profiler) Fields() []protocol.FieldProfile {
	out := make([]protocol.FieldProfile, 0, len(p.fields))
	for _, f := range p.fields {
		fp := protocol.FieldProfile{Path: f.path, Types: f.types, Examples: f.examples}
		if f.examples == nil {
			fp.Examples = []any{}
		}
		// A top-level or nested scalar field is unique when every sampled
		// document had a distinct non-null value. Array element paths are
		// per element and never claim uniqueness.
		if f.unique && f.nonNull > 0 && !strings.Contains(f.path, "[]") && f.nonNull == p.docs {
			fp.Unique = true
		}
		out = append(out, fp)
	}
	return out
}

func truncate(s string) string {
	if utf8.RuneCountInString(s) <= ExampleLen {
		return s
	}
	r := []rune(s)
	return string(r[:ExampleLen])
}

// ParseJSONText reports whether s is the text of a JSON object or array and
// returns the parsed value (numbers as int64/float64). This is how a
// LONGTEXT column holding {"firstname":"Tommy"} gets its inner paths.
func ParseJSONText(s string) (any, bool) {
	if len(s) > MaxJSONText {
		return nil, false
	}
	t := strings.TrimSpace(s)
	if len(t) < 2 {
		return nil, false
	}
	first, last := t[0], t[len(t)-1]
	if !((first == '{' && last == '}') || (first == '[' && last == ']')) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(t)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	switch v.(type) {
	case map[string]any, []any:
		return protocol.NormalizeNumbers(v), true
	}
	return nil, false
}
