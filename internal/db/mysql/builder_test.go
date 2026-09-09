package mysql

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

func check(t *testing.T, name string, got Query, err error, wantSQL string, wantArgs ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if got.SQL != wantSQL {
		t.Errorf("%s SQL:\n got  %s\n want %s", name, got.SQL, wantSQL)
	}
	if wantArgs == nil {
		wantArgs = []any{}
	}
	if got.Args == nil {
		got.Args = []any{}
	}
	if !reflect.DeepEqual(got.Args, wantArgs) {
		t.Errorf("%s args:\n got  %#v\n want %#v", name, got.Args, wantArgs)
	}
}

func TestBuildFind(t *testing.T) {
	cols, err := BaseColumns([]string{"citizenid", "charinfo.firstname", "charinfo.lastname", "money.bank", "job.name"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cols, []string{"citizenid", "charinfo", "money", "job"}) {
		t.Fatalf("base columns: %v", cols)
	}
	q, err := BuildFind("players", cols,
		protocol.Cond("money.bank", "gte", int64(50000)),
		[]protocol.SortSpec{{Field: "money.bank", Dir: "desc"}, {Field: "citizenid", Dir: "asc"}}, 201, 0)
	check(t, "find", q, err,
		"SELECT `citizenid`, `charinfo`, `money`, `job` FROM `players`"+
			" WHERE (JSON_UNQUOTE(JSON_EXTRACT(`money`, '$.\"bank\"')) + 0.0) >= ?"+
			" ORDER BY (JSON_UNQUOTE(JSON_EXTRACT(`money`, '$.\"bank\"')) + 0.0) DESC, JSON_UNQUOTE(JSON_EXTRACT(`money`, '$.\"bank\"')) DESC, `citizenid` ASC"+
			" LIMIT ? OFFSET ?",
		int64(50000), 201, 0)

	q, err = BuildFind("players", []string{"id"}, nil, nil, 101, 200)
	check(t, "plain", q, err, "SELECT `id` FROM `players` LIMIT ? OFFSET ?", 101, 200)
}

func TestWhereOperators(t *testing.T) {
	cases := []struct {
		name string
		f    *protocol.Filter
		sql  string
		args []any
	}{
		{"eq", protocol.Cond("bank", "eq", int64(1)), "`bank` = ?", []any{int64(1)}},
		{"eq null", protocol.Cond("bank", "eq", nil), "`bank` IS NULL", nil},
		{"eq null nested", protocol.Cond("m.b", "eq", nil), "(JSON_EXTRACT(`m`, '$.\"b\"') IS NULL OR JSON_TYPE(JSON_EXTRACT(`m`, '$.\"b\"')) = 'NULL')", nil},
		{"ne", protocol.Cond("bank", "ne", "x"), "(`bank` <> ? OR `bank` IS NULL)", []any{"x"}},
		{"ne null", protocol.Cond("bank", "ne", nil), "`bank` IS NOT NULL", nil},
		{"gt text", protocol.Cond("name", "gt", "m"), "`name` > ?", []any{"m"}},
		{"lte nested number", protocol.Cond("m.b", "lte", 2.5), "(JSON_UNQUOTE(JSON_EXTRACT(`m`, '$.\"b\"')) + 0.0) <= ?", []any{2.5}},
		{"lt nested text", protocol.Cond("c.n", "lt", "z"), "JSON_UNQUOTE(JSON_EXTRACT(`c`, '$.\"n\"')) < ?", []any{"z"}},
		{"in", protocol.Cond("job", "in", []any{"a", "b"}), "`job` IN (?, ?)", []any{"a", "b"}},
		{"in with null", protocol.Cond("job", "in", []any{"a", nil}), "(`job` IN (?) OR `job` IS NULL)", []any{"a"}},
		{"in empty", protocol.Cond("job", "in", []any{}), "1 = 0", nil},
		{"in only null", protocol.Cond("job", "in", []any{nil}), "`job` IS NULL", nil},
		{"nin", protocol.Cond("job", "nin", []any{"a"}), "(`job` NOT IN (?) OR `job` IS NULL)", []any{"a"}},
		{"nin with null", protocol.Cond("job", "nin", []any{"a", nil}), "(`job` NOT IN (?) AND `job` IS NOT NULL)", []any{"a"}},
		{"nin empty", protocol.Cond("job", "nin", []any{}), "1 = 1", nil},
		{"contains", protocol.Cond("name", "contains", "50%_|x"), "`name` LIKE ? ESCAPE '|'", []any{"%50|%|_||x%"}},
		{"starts nested", protocol.Cond("c.first", "starts", "To"), "JSON_UNQUOTE(JSON_EXTRACT(`c`, '$.\"first\"')) LIKE ? ESCAPE '|'", []any{"To%"}},
		{"exists", protocol.Cond("bank", "exists", true), "`bank` IS NOT NULL", nil},
		{"not exists", protocol.Cond("bank", "exists", false), "`bank` IS NULL", nil},
		{"exists nested", protocol.Cond("c.first", "exists", true), "JSON_CONTAINS_PATH(`c`, 'one', '$.\"first\"')", nil},
		{"not exists nested", protocol.Cond("c.first", "exists", false), "NOT JSON_CONTAINS_PATH(`c`, 'one', '$.\"first\"')", nil},
		{"deep path", protocol.Cond("a.b-c.d_e", "eq", int64(1)), "JSON_UNQUOTE(JSON_EXTRACT(`a`, '$.\"b-c\".\"d_e\"')) = ?", []any{int64(1)}},
		{"and or not", protocol.And(protocol.Cond("a", "eq", int64(1)), protocol.Or(protocol.Cond("b", "eq", int64(2)), protocol.Not(protocol.Cond("c", "eq", int64(3))))),
			"(`a` = ? AND (`b` = ? OR NOT `c` = ?))", []any{int64(1), int64(2), int64(3)}},
	}
	for _, c := range cases {
		var args []any
		where, err := whereClause(c.f, &args)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if where != " WHERE "+c.sql {
			t.Errorf("%s:\n got  %s\n want %s", c.name, strings.TrimPrefix(where, " WHERE "), c.sql)
		}
		if c.args == nil {
			c.args = []any{}
		}
		if args == nil {
			args = []any{}
		}
		if !reflect.DeepEqual(args, c.args) {
			t.Errorf("%s args: got %#v want %#v", c.name, args, c.args)
		}
	}
}

func TestBuildCount(t *testing.T) {
	q, err := BuildCount("players", nil)
	check(t, "count all", q, err, "SELECT COUNT(*) FROM `players`")
	q, err = BuildCount("players", protocol.Cond("job", "eq", "police"))
	check(t, "count where", q, err, "SELECT COUNT(*) FROM `players` WHERE `job` = ?", "police")
}

func TestBuildAggregate(t *testing.T) {
	metrics := []protocol.Metric{{Fn: "count", As: "count"}, {Fn: "sum", Field: "money.bank", As: "bank_total"}, {Fn: "avg", Field: "level", As: "avg_level"}}
	q, err := BuildAggregate("players", protocol.Cond("bank", "gt", int64(0)), "job.name", metrics, 50)
	check(t, "grouped", q, err,
		"SELECT JSON_UNQUOTE(JSON_EXTRACT(`job`, '$.\"name\"')) AS `group`, COUNT(*) AS `count`, SUM((JSON_UNQUOTE(JSON_EXTRACT(`money`, '$.\"bank\"')) + 0.0)) AS `bank_total`, AVG(`level`) AS `avg_level`"+
			" FROM `players` WHERE `bank` > ? GROUP BY `group` ORDER BY `count` DESC LIMIT ?",
		int64(0), 50)

	q, err = BuildAggregate("players", nil, "", []protocol.Metric{{Fn: "max", Field: "bank", As: "max_bank"}}, 50)
	check(t, "ungrouped", q, err, "SELECT NULL AS `group`, MAX(`bank`) AS `max_bank` FROM `players` ORDER BY `group` ASC LIMIT ?", 50)

	q, err = BuildAggregate("players", nil, "job", []protocol.Metric{{Fn: "count", As: "n"}}, 10)
	check(t, "count alias", q, err, "SELECT `job` AS `group`, COUNT(*) AS `n` FROM `players` GROUP BY `group` ORDER BY `n` DESC LIMIT ?", 10)
}

func TestBuildWrites(t *testing.T) {
	q, err := BuildInsert("bans", map[string]any{"reason": "RDM", "license": "abc", "expire": int64(0)})
	check(t, "insert", q, err, "INSERT INTO `bans` (`expire`, `license`, `reason`) VALUES (?, ?, ?)", int64(0), "abc", "RDM")

	q, err = BuildUpdate("players", map[string]any{"job": "police"}, protocol.Cond("citizenid", "eq", "ABC"), 1)
	check(t, "update", q, err, "UPDATE `players` SET `job` = ? WHERE `citizenid` = ? LIMIT ?", "police", "ABC", 1)

	q, err = BuildDelete("bans", protocol.Cond("license", "eq", "abc"), 5)
	check(t, "delete", q, err, "DELETE FROM `bans` WHERE `license` = ? LIMIT ?", "abc", 5)

	if _, err := BuildUpdate("players", map[string]any{"job": "x"}, nil, 1); db.CodeOf(err) != protocol.ErrRefused {
		t.Errorf("update without filter must be refused, got %v", err)
	}
	if _, err := BuildDelete("players", nil, 1); db.CodeOf(err) != protocol.ErrRefused {
		t.Errorf("delete without filter must be refused, got %v", err)
	}
	if _, err := BuildInsert("bans", map[string]any{"charinfo.firstname": "x"}); db.CodeOf(err) != protocol.ErrUnsupported {
		t.Errorf("nested insert key must be unsupported, got %v", err)
	}
}

func TestInjectionRejected(t *testing.T) {
	bad := []string{"players; DROP TABLE users", "players`", "play ers", "a-b", "1abc", "", "x\x00", strings.Repeat("a", 65)}
	for _, name := range bad {
		if _, err := BuildCount(name, nil); db.CodeOf(err) != protocol.ErrInvalid {
			t.Errorf("table %q should be invalid, got %v", name, err)
		}
		if _, err := BaseColumns([]string{name}); db.CodeOf(err) != protocol.ErrInvalid {
			t.Errorf("column %q should be invalid, got %v", name, err)
		}
		if _, err := BuildInsert("t", map[string]any{name: 1}); db.CodeOf(err) != protocol.ErrInvalid {
			t.Errorf("insert column %q should be invalid, got %v", name, err)
		}
	}
	// A dash is legal in a path segment but not in a MySQL identifier.
	var args []any
	if _, err := whereClause(protocol.Cond("a-b", "eq", int64(1)), &args); db.CodeOf(err) != protocol.ErrInvalid {
		t.Errorf("dashed column should be invalid, got %v", err)
	}
	// Array paths are unsupported in filters and sorts.
	if _, err := whereClause(protocol.Cond("inventory[].count", "eq", int64(1)), &args); db.CodeOf(err) != protocol.ErrUnsupported {
		t.Errorf("array path should be unsupported, got %v", err)
	}
	if _, err := orderBy([]protocol.SortSpec{{Field: "inventory[]", Dir: "asc"}}); db.CodeOf(err) != protocol.ErrUnsupported {
		t.Errorf("array sort should be unsupported, got %v", err)
	}
	// Values never appear in the SQL text.
	q, _ := BuildCount("t", protocol.Cond("name", "eq", "' OR 1=1 --"))
	if strings.Contains(q.SQL, "OR 1=1") {
		t.Errorf("value leaked into SQL: %s", q.SQL)
	}
}

func TestParseURL(t *testing.T) {
	cfg, err := ParseURL("mysql://fivepanel_ro:p%40ss@10.0.0.5:3307/qbcore?charset=utf8mb4&timeout=5s")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "fivepanel_ro" || cfg.Passwd != "p@ss" || cfg.Addr != "10.0.0.5:3307" || cfg.DBName != "qbcore" || !cfg.ParseTime || !cfg.ClientFoundRows {
		t.Errorf("url parse: %+v", cfg)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("driver parameters not kept: timeout=%v", cfg.Timeout)
	}
	cfg, err = ParseURL("user:pass@tcp(127.0.0.1:3306)/db?parseTime=false")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBName != "db" || cfg.Addr != "127.0.0.1:3306" || !cfg.ParseTime {
		t.Errorf("dsn parse: %+v", cfg)
	}
	cfg, err = ParseURL("mysql://u:p@localhost/db")
	if err != nil || cfg.Addr != "localhost:3306" {
		t.Errorf("default port: %+v %v", cfg, err)
	}
	if _, err := ParseURL("mysql://u:p@localhost/"); err == nil {
		t.Error("missing database should fail")
	}
	host, name := Describe("mysql://u:secret@h:1/d")
	if host != "h:1" || name != "d" {
		t.Errorf("describe: %s %s", host, name)
	}
}

func TestConvert(t *testing.T) {
	if v := convert([]byte("12.50"), "DECIMAL"); v != 12.5 {
		t.Errorf("decimal: %#v", v)
	}
	if v := convert([]byte("42"), "BIGINT"); v != int64(42) {
		t.Errorf("bigint: %#v", v)
	}
	if v := convert([]byte{1}, "BIT"); v != true {
		t.Errorf("bit: %#v", v)
	}
	if v, ok := convert([]byte{0, 1}, "BLOB").([]byte); !ok || len(v) != 2 {
		t.Errorf("blob: %#v", v)
	}
	if v := convert([]byte(`{"a":1}`), "TEXT"); v != `{"a":1}` {
		t.Errorf("text: %#v", v)
	}
	if v := convert(int64(7), "INT"); v != int64(7) {
		t.Errorf("passthrough: %#v", v)
	}
}
