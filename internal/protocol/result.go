// Results (query_res). One struct covers every shape; the constructors set
// exactly the payload field the operation answers with, and MarshalJSON
// emits only that field so an error answer does not carry an empty rows
// list and a list answer does not carry a null count.
package protocol

import "encoding/json"

// QueryError is the error payload of a failed request.
type QueryError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CollectionInfo is one entry of a list answer.
type CollectionInfo struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// FieldProfile describes one field path of a profiled collection.
type FieldProfile struct {
	Path     string         `json:"path"`
	Types    map[string]int `json:"types"`
	Examples []any          `json:"examples"`
	Unique   bool           `json:"unique,omitempty"`
}

// CollectionProfile is the profile answer.
type CollectionProfile struct {
	Collection string         `json:"collection"`
	Sampled    int            `json:"sampled"`
	Count      int64          `json:"count"`
	Fields     []FieldProfile `json:"fields"`
}

// Row is one flat result row: requested path -> JSON scalar.
type Row map[string]any

// QueryRes is the answer to a request.
type QueryRes struct {
	Type            string `json:"type"`
	QueryID         string `json:"query_id"`
	Status          string `json:"status"`
	ExecutionTimeMs int64  `json:"execution_time_ms"`

	Error       *QueryError        `json:"error,omitempty"`
	Collections []CollectionInfo   `json:"collections,omitempty"`
	Profile     *CollectionProfile `json:"profile,omitempty"`
	Rows        []Row              `json:"rows,omitempty"`
	Truncated   *bool              `json:"truncated,omitempty"`
	Count       *int64             `json:"count,omitempty"`
	Affected    *int64             `json:"affected,omitempty"`
}

func okRes(id string) *QueryRes { return &QueryRes{Type: TypeQueryRes, QueryID: id, Status: "ok"} }

// NewError builds a failed answer.
func NewError(id, code, message string) *QueryRes {
	return &QueryRes{Type: TypeQueryRes, QueryID: id, Status: "error", Error: &QueryError{Code: code, Message: message}}
}

// NewList builds a list answer.
func NewList(id string, cols []CollectionInfo) *QueryRes {
	if cols == nil {
		cols = []CollectionInfo{}
	}
	r := okRes(id)
	r.Collections = cols
	return r
}

// NewProfile builds a profile answer.
func NewProfile(id string, p *CollectionProfile) *QueryRes {
	r := okRes(id)
	r.Profile = p
	return r
}

// NewRows builds a find answer (truncated set) or an aggregate answer
// (truncated nil).
func NewRows(id string, rows []Row, truncated *bool) *QueryRes {
	if rows == nil {
		rows = []Row{}
	}
	r := okRes(id)
	r.Rows = rows
	r.Truncated = truncated
	return r
}

// NewCount builds a count answer.
func NewCount(id string, n int64) *QueryRes {
	r := okRes(id)
	r.Count = &n
	return r
}

// NewAffected builds a write answer.
func NewAffected(id string, n int64) *QueryRes {
	r := okRes(id)
	r.Affected = &n
	return r
}

// MarshalJSON writes the base fields plus whichever payload is present.
// Rows and Collections are written even when empty (as []), which the
// omitempty tags alone would not do.
func (r *QueryRes) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"type":              TypeQueryRes,
		"query_id":          r.QueryID,
		"status":            r.Status,
		"execution_time_ms": r.ExecutionTimeMs,
	}
	switch {
	case r.Error != nil:
		out["error"] = r.Error
	case r.Collections != nil:
		out["collections"] = r.Collections
	case r.Profile != nil:
		out["profile"] = r.Profile
	case r.Rows != nil:
		out["rows"] = r.Rows
		if r.Truncated != nil {
			out["truncated"] = *r.Truncated
		}
	case r.Count != nil:
		out["count"] = *r.Count
	case r.Affected != nil:
		out["affected"] = *r.Affected
	}
	return json.Marshal(out)
}
