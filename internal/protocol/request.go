// Requests (query_req) and their validation. Validate is a port of
// requestProblem() in protocol.ts and runs on every request before the
// executor looks at the database, so a bad request costs nothing but a
// query_res with status "error" and code "invalid".
package protocol

import (
	"fmt"
)

// SortSpec is one sort key of a find.
type SortSpec struct {
	Field string `json:"field"`
	Dir   string `json:"dir"`
}

// Metric is one aggregate function of an aggregate request.
type Metric struct {
	Fn    string `json:"fn"`
	Field string `json:"field,omitempty"`
	As    string `json:"as,omitempty"`
}

// QueryReq is every request shape folded into one struct; Op says which
// fields matter. Pointers distinguish "absent" from "zero" where the doc
// gives a default.
type QueryReq struct {
	Type       string         `json:"type"`
	QueryID    string         `json:"query_id"`
	Op         string         `json:"op"`
	TimeoutMs  int            `json:"timeout_ms"`
	Collection string         `json:"collection,omitempty"`
	Sample     *int           `json:"sample,omitempty"`
	Examples   *int           `json:"examples,omitempty"`
	Fields     []string       `json:"fields,omitempty"`
	Filter     *Filter        `json:"filter,omitempty"`
	Sort       []SortSpec     `json:"sort,omitempty"`
	Limit      *int           `json:"limit,omitempty"`
	Skip       *int           `json:"skip,omitempty"`
	GroupBy    *string        `json:"group_by,omitempty"`
	Metrics    []Metric       `json:"metrics,omitempty"`
	Action     string         `json:"action,omitempty"`
	Values     map[string]any `json:"values,omitempty"`
	Set        map[string]any `json:"set,omitempty"`

	// decodeProblem is set by Decode when the frame named a query_req but
	// some field had the wrong JSON type; Validate reports it.
	decodeProblem string
}

// Validate returns why the request is not acceptable, or "". It checks
// shape and limits only; whether the engine supports a construct (an array
// path in a MySQL filter, say) is the engine's call.
func (q *QueryReq) Validate() string {
	if q.decodeProblem != "" {
		return q.decodeProblem
	}
	if q.QueryID == "" {
		return "query_id is required"
	}
	if q.TimeoutMs != 0 && (q.TimeoutMs < MinTimeoutMs || q.TimeoutMs > MaxTimeoutMs) {
		return "timeout out of range"
	}
	if q.Op == OpList {
		return ""
	}
	if !IsCollectionName(q.Collection) {
		return "bad collection name"
	}
	if q.Filter != nil {
		if p := ValidateFilter(q.Filter); p != "" {
			return p
		}
	}
	switch q.Op {
	case OpProfile:
		if q.Sample != nil && (*q.Sample < 1 || *q.Sample > MaxSample) {
			return "sample out of range"
		}
		if q.Examples != nil && (*q.Examples < 0 || *q.Examples > MaxExamples) {
			return "examples out of range"
		}
		return ""
	case OpFind:
		if len(q.Fields) == 0 || len(q.Fields) > MaxFields {
			return "fields must be 1 to 64 paths"
		}
		for _, f := range q.Fields {
			if !IsPath(f) {
				return "fields must be 1 to 64 paths"
			}
		}
		if len(q.Sort) > MaxSortKeys {
			return "bad sort"
		}
		for _, s := range q.Sort {
			if !IsPath(s.Field) || (s.Dir != "asc" && s.Dir != "desc") {
				return "bad sort"
			}
		}
		if q.Limit != nil && (*q.Limit < 1 || *q.Limit > MaxRows) {
			return "limit out of range"
		}
		if q.Skip != nil && (*q.Skip < 0 || *q.Skip > MaxSkip) {
			return "skip out of range"
		}
		return ""
	case OpCount:
		return ""
	case OpAggregate:
		if q.GroupBy != nil && !IsPath(*q.GroupBy) {
			return "bad group_by"
		}
		if len(q.Metrics) == 0 || len(q.Metrics) > MaxMetrics {
			return "metrics must be 1 to 8 entries"
		}
		keys := map[string]bool{"group": true}
		for _, m := range q.Metrics {
			if !contains(MetricFns, m.Fn) {
				return "bad metric"
			}
			if m.Fn != "count" && !IsPath(m.Field) {
				return fmt.Sprintf("%q needs a field", m.Fn)
			}
			if m.As != "" && !IsIdentifier(m.As) {
				return "bad metric name"
			}
			key := MetricKey(m)
			if keys[key] {
				return fmt.Sprintf("duplicate metric name %q", key)
			}
			keys[key] = true
		}
		if q.Limit != nil && (*q.Limit < 1 || *q.Limit > MaxRows) {
			return "limit out of range"
		}
		return ""
	case OpWrite:
		switch q.Action {
		case ActionInsert:
			if p := scalarMapProblem(q.Values); p != "" {
				return "insert needs values: " + p
			}
		case ActionUpdate:
			if p := scalarMapProblem(q.Set); p != "" {
				return "update needs set: " + p
			}
		case ActionDelete:
		default:
			return "bad action"
		}
		if q.Limit != nil && (*q.Limit < 1 || *q.Limit > MaxWriteLimit) {
			return "limit out of range"
		}
		return ""
	}
	return "unknown operation"
}

// scalarMapProblem checks a values/set map: at least one entry, every key a
// field path, every value a scalar.
func scalarMapProblem(m map[string]any) string {
	if len(m) == 0 {
		return "an object with at least one field"
	}
	for k, v := range m {
		if !IsPath(k) {
			return fmt.Sprintf("%q is not a field path", k)
		}
		if !IsScalar(v) {
			return fmt.Sprintf("%q must be text, a number, a boolean or null", k)
		}
	}
	return ""
}

// Timeout is the request's timeout in ms with the default applied.
func (q *QueryReq) Timeout() int {
	if q.TimeoutMs <= 0 {
		return DefaultTimeoutMs
	}
	return q.TimeoutMs
}

// IntOr returns *p or def when p is nil.
func IntOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// StringOr returns *p or def when p is nil.
func StringOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}
