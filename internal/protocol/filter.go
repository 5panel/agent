// The filter tree. A filter is either a condition {field, op, value} or one
// of {and: [...]}, {or: [...]}, {not: {...}}. The JSON codec here is strict
// about that shape, and normalises numbers to int64/float64 so the engines
// never see json.Number. A condition value that is an object is not a
// scalar and fails validation; that rule is what stops a `{ "$ne": null }`
// from ever reaching MongoDB, whatever the engine does with the tree.
package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Filter is one node of the tree. Exactly one of And, Or, Not or the
// condition fields (Field/Op/Value) is in use.
type Filter struct {
	And []*Filter
	Or  []*Filter
	Not *Filter

	Field string
	Op    string
	Value any

	// problem records a structural defect found while decoding, reported by
	// Validate so a malformed filter answers "invalid" with a reason instead
	// of failing the whole frame.
	problem string
}

// Cond builds a condition node.
func Cond(field, op string, value any) *Filter {
	return &Filter{Field: field, Op: op, Value: value}
}

// And builds an and-node.
func And(children ...*Filter) *Filter { return &Filter{And: children} }

// Or builds an or-node.
func Or(children ...*Filter) *Filter { return &Filter{Or: children} }

// Not builds a not-node.
func Not(child *Filter) *Filter { return &Filter{Not: child} }

// IsCondition reports whether the node is a leaf condition.
func (f *Filter) IsCondition() bool { return f.And == nil && f.Or == nil && f.Not == nil }

// MarshalJSON renders the node in wire shape.
func (f *Filter) MarshalJSON() ([]byte, error) {
	switch {
	case f.And != nil:
		return json.Marshal(map[string]any{"and": f.And})
	case f.Or != nil:
		return json.Marshal(map[string]any{"or": f.Or})
	case f.Not != nil:
		return json.Marshal(map[string]any{"not": f.Not})
	default:
		return json.Marshal(struct {
			Field string `json:"field"`
			Op    string `json:"op"`
			Value any    `json:"value"`
		}{f.Field, f.Op, f.Value})
	}
}

// UnmarshalJSON parses the wire shape. Structural problems are kept in the
// node rather than returned, so the request as a whole still decodes and
// the gateway gets a query_res with a message.
func (f *Filter) UnmarshalJSON(b []byte) error {
	*f = Filter{}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		f.problem = "a filter must be an object"
		return nil
	}
	for _, key := range []string{"and", "or"} {
		list, ok := raw[key]
		if !ok {
			continue
		}
		if len(raw) != 1 {
			f.problem = `"` + key + `" must be the only key`
			return nil
		}
		var children []*Filter
		if err := json.Unmarshal(list, &children); err != nil || len(children) == 0 {
			f.problem = `"` + key + `" must be a non-empty list`
			return nil
		}
		if key == "and" {
			f.And = children
		} else {
			f.Or = children
		}
		return nil
	}
	if inner, ok := raw["not"]; ok {
		if len(raw) != 1 {
			f.problem = `"not" must be the only key`
			return nil
		}
		var child Filter
		if err := json.Unmarshal(inner, &child); err != nil {
			f.problem = "a filter must be an object"
			return nil
		}
		f.Not = &child
		return nil
	}
	if err := json.Unmarshal(raw["field"], &f.Field); err != nil || f.Field == "" {
		f.problem = "a condition needs a field path"
		return nil
	}
	if err := json.Unmarshal(raw["op"], &f.Op); err != nil {
		f.problem = "a condition needs an operator"
		return nil
	}
	value, ok := raw["value"]
	if !ok {
		f.problem = "a condition needs a value"
		return nil
	}
	v, err := decodeScalarish(value)
	if err != nil {
		f.problem = "a value must be text, a number, a boolean or null"
		return nil
	}
	f.Value = v
	return nil
}

// decodeScalarish decodes JSON into Go values with numbers as int64 or
// float64 rather than json.Number/float64-only.
func decodeScalarish(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return NormalizeNumbers(v), nil
}

// NormalizeNumbers walks a decoded JSON value and turns every json.Number
// into an int64 when it is integral and fits, otherwise a float64.
func NormalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		for i := range x {
			x[i] = NormalizeNumbers(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = NormalizeNumbers(x[k])
		}
		return x
	}
	return v
}

// IsScalar reports whether v is a JSON scalar as the doc defines it: text,
// number, boolean or null.
func IsScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, int, int32, int64, float32, float64:
		return true
	}
	return false
}

// filterProblem is the port of filterProblem() in protocol.ts.
func filterProblem(f *Filter, depth int, seen *int) string {
	if depth > MaxFilterDepth {
		return "filter nests too deep"
	}
	*seen++
	if *seen > MaxFilterNodes {
		return "filter has too many conditions"
	}
	if f == nil {
		return "a filter must be an object"
	}
	if f.problem != "" {
		return f.problem
	}
	switch {
	case f.And != nil:
		for _, c := range f.And {
			if p := filterProblem(c, depth+1, seen); p != "" {
				return p
			}
		}
		return ""
	case f.Or != nil:
		for _, c := range f.Or {
			if p := filterProblem(c, depth+1, seen); p != "" {
				return p
			}
		}
		return ""
	case f.Not != nil:
		return filterProblem(f.Not, depth+1, seen)
	}
	if !IsPath(f.Field) {
		return "a condition needs a field path"
	}
	if !contains(FilterOps, f.Op) {
		return `unknown operator "` + f.Op + `"`
	}
	switch f.Op {
	case "in", "nin":
		list, ok := f.Value.([]any)
		if !ok || len(list) > MaxInValues {
			return `"` + f.Op + `" needs a list of up to 100 values`
		}
		for _, v := range list {
			if !IsScalar(v) {
				return `"` + f.Op + `" needs a list of up to 100 values`
			}
		}
		return ""
	case "exists":
		if _, ok := f.Value.(bool); !ok {
			return `"exists" needs true or false`
		}
		return ""
	case "contains", "starts":
		if _, ok := f.Value.(string); !ok {
			return `"` + f.Op + `" needs text`
		}
		return ""
	}
	if !IsScalar(f.Value) {
		return "a value must be text, a number, a boolean or null"
	}
	return ""
}

// ValidateFilter returns why the tree is not acceptable, or "".
func ValidateFilter(f *Filter) string {
	seen := 0
	return filterProblem(f, 0, &seen)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// String renders the tree compactly for logs and test failures.
func (f *Filter) String() string {
	b, err := json.Marshal(f)
	if err != nil {
		return "<filter>"
	}
	return strings.TrimSpace(string(b))
}
