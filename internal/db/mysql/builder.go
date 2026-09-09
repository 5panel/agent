// The SQL builder. Everything that reaches the server is assembled here
// from three kinds of parts, and nothing else:
//
//   - identifiers (table and column names) that matched
//     ^[A-Za-z_][A-Za-z0-9_]{0,63}$ and are wrapped in backticks;
//   - JSON path literals built from validated path segments, which can
//     only contain letters, digits, "_" and "-";
//   - "?" placeholders, with every value passed as a query parameter.
//
// No request value is ever concatenated into the SQL text. Nested paths
// (charinfo.firstname) become JSON_EXTRACT on the first segment's column,
// which works on JSON columns and on TEXT columns holding JSON alike.
package mysql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

// Query is a statement and its parameters.
type Query struct {
	SQL  string
	Args []any
}

// SampleRandomBelow is the row count under which profile samples with
// ORDER BY RAND(); above it the first rows are read.
const SampleRandomBelow = 50_000

func quote(ident string) string { return "`" + ident + "`" }

// tableName validates and quotes a collection name.
func tableName(collection string) (string, error) {
	if !protocol.IsIdentifier(collection) {
		return "", db.Invalid("%q is not a valid MySQL table name", collection)
	}
	return quote(collection), nil
}

// fieldRef is a validated path rendered as SQL expressions.
type fieldRef struct {
	path     string
	col      string // quoted column
	nested   bool
	jsonPath string // $."a"."b", built from validated segments only
}

func newFieldRef(path string) (fieldRef, error) {
	segs := protocol.ParsePath(path)
	if !protocol.IsIdentifier(segs[0].Name) {
		return fieldRef{}, db.Invalid("%q is not a valid MySQL column name", segs[0].Name)
	}
	for _, s := range segs {
		if s.Array {
			return fieldRef{}, db.Unsupported("array paths such as %q are not supported in MySQL filters, sorts and metrics", path)
		}
	}
	r := fieldRef{path: path, col: quote(segs[0].Name)}
	if len(segs) > 1 {
		r.nested = true
		var b strings.Builder
		b.WriteString("$")
		for _, s := range segs[1:] {
			b.WriteString(`."`)
			b.WriteString(s.Name)
			b.WriteString(`"`)
		}
		r.jsonPath = b.String()
	}
	return r, nil
}

// raw is the JSON value at the path (JSON_EXTRACT) for nested refs.
func (r fieldRef) raw() string {
	return fmt.Sprintf("JSON_EXTRACT(%s, '%s')", r.col, r.jsonPath)
}

// text is the column itself, or the unquoted JSON value for nested refs.
func (r fieldRef) text() string {
	if !r.nested {
		return r.col
	}
	return "JSON_UNQUOTE(" + r.raw() + ")"
}

// numeric coerces a nested value to a number so that comparisons and sorts
// on money.bank are numeric on MySQL and MariaDB alike; a top-level column
// keeps its own type.
func (r fieldRef) numeric() string {
	if !r.nested {
		return r.col
	}
	return "(" + r.text() + " + 0.0)"
}

func (r fieldRef) isNull(null bool) string {
	if !r.nested {
		if null {
			return r.col + " IS NULL"
		}
		return r.col + " IS NOT NULL"
	}
	// A JSON null is not an SQL NULL; treat both as "no value".
	if null {
		return fmt.Sprintf("(%s IS NULL OR JSON_TYPE(%s) = 'NULL')", r.raw(), r.raw())
	}
	return fmt.Sprintf("(%s IS NOT NULL AND JSON_TYPE(%s) <> 'NULL')", r.raw(), r.raw())
}

func isNumber(v any) bool {
	switch v.(type) {
	case int, int32, int64, float32, float64:
		return true
	}
	return false
}

// escapeLike makes a LIKE pattern match the text literally; "|" is the
// escape character so the statement does not depend on the server's
// backslash mode.
func escapeLike(s string) string {
	return strings.NewReplacer("|", "||", "%", "|%", "_", "|_").Replace(s)
}

// whereClause renders " WHERE …" (or "" for a nil filter).
func whereClause(f *protocol.Filter, args *[]any) (string, error) {
	if f == nil {
		return "", nil
	}
	expr, err := compileFilter(f, args)
	if err != nil {
		return "", err
	}
	return " WHERE " + expr, nil
}

func compileFilter(f *protocol.Filter, args *[]any) (string, error) {
	join := func(list []*protocol.Filter, sep string) (string, error) {
		parts := make([]string, 0, len(list))
		for _, c := range list {
			p, err := compileFilter(c, args)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return "(" + strings.Join(parts, sep) + ")", nil
	}
	switch {
	case f.And != nil:
		return join(f.And, " AND ")
	case f.Or != nil:
		return join(f.Or, " OR ")
	case f.Not != nil:
		inner, err := compileFilter(f.Not, args)
		if err != nil {
			return "", err
		}
		return "NOT " + inner, nil
	}
	return compileCondition(f, args)
}

var compareOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<="}

func compileCondition(f *protocol.Filter, args *[]any) (string, error) {
	ref, err := newFieldRef(f.Field)
	if err != nil {
		return "", err
	}
	x := ref.text()
	switch f.Op {
	case "eq":
		if f.Value == nil {
			return ref.isNull(true), nil
		}
		*args = append(*args, f.Value)
		return x + " = ?", nil
	case "ne":
		if f.Value == nil {
			return ref.isNull(false), nil
		}
		*args = append(*args, f.Value)
		return "(" + x + " <> ? OR " + x + " IS NULL)", nil
	case "gt", "gte", "lt", "lte":
		expr := x
		if isNumber(f.Value) {
			expr = ref.numeric()
		}
		*args = append(*args, f.Value)
		return expr + " " + compareOps[f.Op] + " ?", nil
	case "in", "nin":
		return compileIn(f, ref, args)
	case "contains":
		*args = append(*args, "%"+escapeLike(f.Value.(string))+"%")
		return x + " LIKE ? ESCAPE '|'", nil
	case "starts":
		*args = append(*args, escapeLike(f.Value.(string))+"%")
		return x + " LIKE ? ESCAPE '|'", nil
	case "exists":
		want := f.Value.(bool)
		if !ref.nested {
			return ref.isNull(!want), nil
		}
		expr := fmt.Sprintf("JSON_CONTAINS_PATH(%s, 'one', '%s')", ref.col, ref.jsonPath)
		if !want {
			return "NOT " + expr, nil
		}
		return expr, nil
	}
	return "", db.Invalid("unknown operator %q", f.Op)
}

// compileIn handles null inside the list, which SQL's IN cannot.
func compileIn(f *protocol.Filter, ref fieldRef, args *[]any) (string, error) {
	x := ref.text()
	list, _ := f.Value.([]any)
	var values []any
	hasNull := false
	for _, v := range list {
		if v == nil {
			hasNull = true
		} else {
			values = append(values, v)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
	if f.Op == "in" {
		if len(values) == 0 {
			if hasNull {
				return x + " IS NULL", nil
			}
			return "1 = 0", nil
		}
		*args = append(*args, values...)
		s := x + " IN (" + placeholders + ")"
		if hasNull {
			s = "(" + s + " OR " + x + " IS NULL)"
		}
		return s, nil
	}
	if len(values) == 0 {
		if hasNull {
			return x + " IS NOT NULL", nil
		}
		return "1 = 1", nil
	}
	*args = append(*args, values...)
	s := x + " NOT IN (" + placeholders + ")"
	if hasNull {
		return "(" + s + " AND " + x + " IS NOT NULL)", nil
	}
	return "(" + s + " OR " + x + " IS NULL)", nil
}

// orderBy renders " ORDER BY …". A nested path sorts numerically first and
// textually second, so numbers inside JSON order as numbers.
func orderBy(sort []protocol.SortSpec) (string, error) {
	if len(sort) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(sort)*2)
	for _, s := range sort {
		ref, err := newFieldRef(s.Field)
		if err != nil {
			return "", err
		}
		dir := "ASC"
		if s.Dir == "desc" {
			dir = "DESC"
		}
		if ref.nested {
			parts = append(parts, ref.numeric()+" "+dir, ref.text()+" "+dir)
		} else {
			parts = append(parts, ref.col+" "+dir)
		}
	}
	return " ORDER BY " + strings.Join(parts, ", "), nil
}

// BaseColumns returns the distinct first segments of the requested paths,
// validated as identifiers, in first-seen order. Nested values are
// extracted in Go after the fetch, so a find selects only these.
func BaseColumns(fields []string) ([]string, error) {
	seen := map[string]bool{}
	var cols []string
	for _, f := range fields {
		name := protocol.ParsePath(f)[0].Name
		if !protocol.IsIdentifier(name) {
			return nil, db.Invalid("%q is not a valid MySQL column name", name)
		}
		if !seen[name] {
			seen[name] = true
			cols = append(cols, name)
		}
	}
	return cols, nil
}

// BuildFind renders SELECT <columns> FROM <table> [WHERE] [ORDER BY] LIMIT ? OFFSET ?.
func BuildFind(collection string, columns []string, filter *protocol.Filter, sort []protocol.SortSpec, limit, skip int) (Query, error) {
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	quoted := make([]string, 0, len(columns))
	for _, c := range columns {
		if !protocol.IsIdentifier(c) {
			return Query{}, db.Invalid("%q is not a valid MySQL column name", c)
		}
		quoted = append(quoted, quote(c))
	}
	var args []any
	where, err := whereClause(filter, &args)
	if err != nil {
		return Query{}, err
	}
	order, err := orderBy(sort)
	if err != nil {
		return Query{}, err
	}
	args = append(args, limit, skip)
	return Query{
		SQL:  "SELECT " + strings.Join(quoted, ", ") + " FROM " + table + where + order + " LIMIT ? OFFSET ?",
		Args: args,
	}, nil
}

// BuildCount renders SELECT COUNT(*) FROM <table> [WHERE].
func BuildCount(collection string, filter *protocol.Filter) (Query, error) {
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	var args []any
	where, err := whereClause(filter, &args)
	if err != nil {
		return Query{}, err
	}
	return Query{SQL: "SELECT COUNT(*) FROM " + table + where, Args: args}, nil
}

// BuildAggregate renders the GROUP BY statement. Output columns are
// `group` and one per metric, named by Metric.As (already resolved).
func BuildAggregate(collection string, filter *protocol.Filter, groupBy string, metrics []protocol.Metric, limit int) (Query, error) {
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	selects := []string{"NULL AS `group`"}
	if groupBy != "" {
		ref, err := newFieldRef(groupBy)
		if err != nil {
			return Query{}, err
		}
		selects[0] = ref.text() + " AS `group`"
	}
	countKey := ""
	for _, m := range metrics {
		if !protocol.IsIdentifier(m.As) {
			return Query{}, db.Invalid("%q is not a valid metric name", m.As)
		}
		key := quote(m.As)
		if m.Fn == "count" {
			selects = append(selects, "COUNT(*) AS "+key)
			if countKey == "" {
				countKey = key
			}
			continue
		}
		ref, err := newFieldRef(m.Field)
		if err != nil {
			return Query{}, err
		}
		selects = append(selects, fmt.Sprintf("%s(%s) AS %s", strings.ToUpper(m.Fn), ref.numeric(), key))
	}
	var args []any
	where, err := whereClause(filter, &args)
	if err != nil {
		return Query{}, err
	}
	sql := "SELECT " + strings.Join(selects, ", ") + " FROM " + table + where
	if groupBy != "" {
		sql += " GROUP BY `group`"
	}
	if countKey != "" {
		sql += " ORDER BY " + countKey + " DESC"
	} else {
		sql += " ORDER BY `group` ASC"
	}
	sql += " LIMIT ?"
	args = append(args, limit)
	return Query{SQL: sql, Args: args}, nil
}

// columnsOf validates the keys of a values/set map as plain columns and
// returns them sorted, so the statement text is deterministic.
func columnsOf(m map[string]any) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		if !protocol.IsIdentifier(k) {
			if strings.ContainsAny(k, ".[") {
				return nil, db.Unsupported("nested path %q cannot be written on MySQL; write the whole column", k)
			}
			return nil, db.Invalid("%q is not a valid MySQL column name", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// BuildInsert renders INSERT INTO <table> (…) VALUES (…).
func BuildInsert(collection string, values map[string]any) (Query, error) {
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	keys, err := columnsOf(values)
	if err != nil {
		return Query{}, err
	}
	cols := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		cols[i] = quote(k)
		args[i] = values[k]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(keys)), ", ")
	return Query{
		SQL:  "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" + placeholders + ")",
		Args: args,
	}, nil
}

// BuildUpdate renders UPDATE <table> SET … WHERE … LIMIT ?. A nil filter is
// refused here too, in case a caller bypasses the executor.
func BuildUpdate(collection string, set map[string]any, filter *protocol.Filter, limit int) (Query, error) {
	if filter == nil {
		return Query{}, db.Refused("update without a filter is refused")
	}
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	keys, err := columnsOf(set)
	if err != nil {
		return Query{}, err
	}
	assigns := make([]string, len(keys))
	var args []any
	for i, k := range keys {
		assigns[i] = quote(k) + " = ?"
		args = append(args, set[k])
	}
	where, err := whereClause(filter, &args)
	if err != nil {
		return Query{}, err
	}
	args = append(args, limit)
	return Query{SQL: "UPDATE " + table + " SET " + strings.Join(assigns, ", ") + where + " LIMIT ?", Args: args}, nil
}

// BuildDelete renders DELETE FROM <table> WHERE … LIMIT ?.
func BuildDelete(collection string, filter *protocol.Filter, limit int) (Query, error) {
	if filter == nil {
		return Query{}, db.Refused("delete without a filter is refused")
	}
	table, err := tableName(collection)
	if err != nil {
		return Query{}, err
	}
	var args []any
	where, err := whereClause(filter, &args)
	if err != nil {
		return Query{}, err
	}
	args = append(args, limit)
	return Query{SQL: "DELETE FROM " + table + where + " LIMIT ?", Args: args}, nil
}
