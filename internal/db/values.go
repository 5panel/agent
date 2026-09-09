// Value plumbing shared by the engines: turning whatever a driver returned
// into the JSON scalars the protocol allows, and reading a dotted path out
// of a document. Both engines produce documents as map[string]any (with
// []any, string, int64, float64, bool, time.Time, []byte and nil inside),
// which is also what the profiler consumes.
package db

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/5panel/agent/internal/profile"
	"github.com/5panel/agent/internal/protocol"
)

// ToScalar converts a document value to a protocol scalar: strings,
// numbers, booleans and null stay; dates become RFC 3339; binary becomes
// base64; an object or array becomes its JSON text.
func ToScalar(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, int64:
		return x
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case uint:
		return uint64(x)
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return x
	case float32:
		return finite(float64(x))
	case float64:
		return finite(x)
	case json.Number:
		return protocol.NormalizeNumbers(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	case map[string]any, []any:
		return JSONText(x)
	default:
		return fmt.Sprint(x)
	}
}

// finite keeps JSON marshalling from failing on NaN or infinities.
func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

// JSONText renders a container as compact JSON, with dates as RFC 3339 and
// binary as base64 (encoding/json's own rules).
func JSONText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// Extract reads a path out of a document. Containers stored as JSON text
// are parsed on the way down, so "charinfo.firstname" works on a LONGTEXT
// column as well as on a native object. A "[]" segment maps the rest of the
// path over the array's elements and yields the list; an array met without
// "[]" is traversed the same way, which is MongoDB's own behaviour. A
// missing path yields nil.
func Extract(doc any, segs []protocol.Segment) any {
	if len(segs) == 0 {
		return doc
	}
	seg := segs[0]
	obj := asObject(doc)
	if obj == nil {
		return nil
	}
	child, ok := obj[seg.Name]
	if !ok {
		return nil
	}
	rest := segs[1:]
	if arr := asArray(child); arr != nil {
		if len(rest) == 0 {
			// The container itself is the value; keep the original (a JSON
			// text column stays verbatim).
			return child
		}
		out := make([]any, 0, len(arr))
		for _, el := range arr {
			out = append(out, Extract(el, rest))
		}
		return out
	}
	if seg.Array {
		return nil
	}
	return Extract(child, rest)
}

func asObject(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case string:
		if inner, ok := profile.ParseJSONText(x); ok {
			if m, ok := inner.(map[string]any); ok {
				return m
			}
		}
	}
	return nil
}

func asArray(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case string:
		if inner, ok := profile.ParseJSONText(x); ok {
			if a, ok := inner.([]any); ok {
				return a
			}
		}
	}
	return nil
}

// ProjectRow builds a result row from a document for the requested paths.
func ProjectRow(doc map[string]any, fields []string) protocol.Row {
	row := make(protocol.Row, len(fields))
	for _, f := range fields {
		row[f] = ToScalar(Extract(doc, protocol.ParsePath(f)))
	}
	return row
}
