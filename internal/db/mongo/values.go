// BSON to plain Go values. Documents come off the cursor as bson.D and are
// flattened into map[string]any so the executor and the profiler see the
// same shapes the MySQL engine produces: ObjectIDs as hex strings, dates as
// time.Time, binary as []byte, numbers as int64/float64.
package mongo

import (
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// normalizeDoc converts a decoded document.
func normalizeDoc(d bson.D) map[string]any {
	m := make(map[string]any, len(d))
	for _, e := range d {
		m[e.Key] = normalize(e.Value)
	}
	return m
}

// normalize converts one BSON value recursively.
func normalize(v any) any {
	switch x := v.(type) {
	case bson.D:
		return normalizeDoc(x)
	case bson.M:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = normalize(val)
		}
		return m
	case bson.A:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = normalize(el)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = normalize(el)
		}
		return out
	case bson.ObjectID:
		return x.Hex()
	case bson.DateTime:
		return x.Time().UTC()
	case bson.Binary:
		return x.Data
	case bson.Decimal128:
		if f, err := strconv.ParseFloat(x.String(), 64); err == nil {
			return f
		}
		return x.String()
	case bson.Null, bson.Undefined:
		return nil
	case int32:
		return int64(x)
	case bson.Regex:
		return x.Pattern
	case bson.JavaScript:
		return string(x)
	case bson.Symbol:
		return string(x)
	case bson.Timestamp:
		return int64(x.T)
	}
	return v
}

// scalarDoc builds an insert/set document from a validated scalar map with
// deterministic key order.
func scalarDoc(m map[string]any, keys []string) bson.D {
	d := make(bson.D, 0, len(keys))
	for _, k := range keys {
		d = append(d, bson.E{Key: k, Value: m[k]})
	}
	return d
}
