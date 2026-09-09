// Filter tree to bson.D. Operators are emitted by this code as bson keys
// ("$eq", "$in", …); request values only ever appear as operand values,
// and the protocol has already refused any value that is an object. So a
// request cannot introduce an operator of its own, whatever it contains.
package mongo

import (
	"regexp"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

// Compile turns a validated filter into a query document. nil gives the
// empty document (match everything).
func Compile(f *protocol.Filter) (bson.D, error) {
	if f == nil {
		return bson.D{}, nil
	}
	return compile(f)
}

func compile(f *protocol.Filter) (bson.D, error) {
	list := func(children []*protocol.Filter) (bson.A, error) {
		out := make(bson.A, 0, len(children))
		for _, c := range children {
			d, err := compile(c)
			if err != nil {
				return nil, err
			}
			out = append(out, d)
		}
		return out, nil
	}
	switch {
	case f.And != nil:
		a, err := list(f.And)
		if err != nil {
			return nil, err
		}
		return bson.D{{Key: "$and", Value: a}}, nil
	case f.Or != nil:
		a, err := list(f.Or)
		if err != nil {
			return nil, err
		}
		return bson.D{{Key: "$or", Value: a}}, nil
	case f.Not != nil:
		inner, err := compile(f.Not)
		if err != nil {
			return nil, err
		}
		return bson.D{{Key: "$nor", Value: bson.A{inner}}}, nil
	}
	return condition(f)
}

var mongoOps = map[string]string{"eq": "$eq", "ne": "$ne", "gt": "$gt", "gte": "$gte", "lt": "$lt", "lte": "$lte", "in": "$in", "nin": "$nin"}

func condition(f *protocol.Filter) (bson.D, error) {
	field := protocol.StripArrays(f.Field)
	var op bson.D
	switch f.Op {
	case "eq", "ne", "gt", "gte", "lt", "lte":
		op = bson.D{{Key: mongoOps[f.Op], Value: coerceID(field, f.Value)}}
	case "in", "nin":
		raw, _ := f.Value.([]any)
		values := make(bson.A, 0, len(raw))
		for _, v := range raw {
			values = append(values, coerceID(field, v))
		}
		op = bson.D{{Key: mongoOps[f.Op], Value: values}}
	case "contains":
		s, _ := f.Value.(string)
		op = bson.D{{Key: "$regex", Value: regexp.QuoteMeta(s)}, {Key: "$options", Value: "i"}}
	case "starts":
		s, _ := f.Value.(string)
		op = bson.D{{Key: "$regex", Value: "^" + regexp.QuoteMeta(s)}, {Key: "$options", Value: "i"}}
	case "exists":
		b, _ := f.Value.(bool)
		op = bson.D{{Key: "$exists", Value: b}}
	default:
		return nil, db.Invalid("unknown operator %q", f.Op)
	}
	return bson.D{{Key: field, Value: op}}, nil
}

// coerceID turns a 24-hex string compared against _id into an ObjectID,
// since ids travel as hex strings on the wire.
func coerceID(field string, v any) any {
	if field != "_id" {
		return v
	}
	s, ok := v.(string)
	if !ok || len(s) != 24 {
		return v
	}
	id, err := bson.ObjectIDFromHex(s)
	if err != nil {
		return v
	}
	return id
}

// sortDoc renders the sort keys.
func sortDoc(sort []protocol.SortSpec) bson.D {
	d := make(bson.D, 0, len(sort))
	for _, s := range sort {
		dir := 1
		if s.Dir == "desc" {
			dir = -1
		}
		d = append(d, bson.E{Key: protocol.StripArrays(s.Field), Value: dir})
	}
	return d
}

// projection asks MongoDB for the top-level field of every requested
// path and lets db.Extract walk the rest in Go. Projecting the full dotted
// path would be cheaper on the wire, but it returns nothing when the
// container is JSON stored as text ("charinfo" holding a string), which is
// how some servers keep character data in MongoDB too -- and the profiler
// reads those as nested paths, so find must as well. _id is excluded
// unless asked for.
func projection(fields []string) bson.D {
	d := bson.D{}
	seen := map[string]bool{}
	wantID := false
	for _, f := range fields {
		top := protocol.ParsePath(f)[0].Name
		if seen[top] {
			continue
		}
		seen[top] = true
		if top == "_id" {
			wantID = true
		}
		d = append(d, bson.E{Key: top, Value: 1})
	}
	if !wantID {
		d = append(d, bson.E{Key: "_id", Value: 0})
	}
	return d
}
