package mongo

import (
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

func TestCompileFilter(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("66d0a1b2c3d4e5f6a7b8c9d0")
	cases := []struct {
		name string
		f    *protocol.Filter
		want bson.D
	}{
		{"nil", nil, bson.D{}},
		{"eq", protocol.Cond("bank", "eq", int64(5)), bson.D{{Key: "bank", Value: bson.D{{Key: "$eq", Value: int64(5)}}}}},
		{"ne null", protocol.Cond("bank", "ne", nil), bson.D{{Key: "bank", Value: bson.D{{Key: "$ne", Value: nil}}}}},
		{"gte nested", protocol.Cond("money.bank", "gte", 1.5), bson.D{{Key: "money.bank", Value: bson.D{{Key: "$gte", Value: 1.5}}}}},
		{"array path stripped", protocol.Cond("inventory[].count", "gt", int64(1)), bson.D{{Key: "inventory.count", Value: bson.D{{Key: "$gt", Value: int64(1)}}}}},
		{"in", protocol.Cond("job", "in", []any{"a", int64(1), nil}), bson.D{{Key: "job", Value: bson.D{{Key: "$in", Value: bson.A{"a", int64(1), nil}}}}}},
		{"nin", protocol.Cond("job", "nin", []any{"a"}), bson.D{{Key: "job", Value: bson.D{{Key: "$nin", Value: bson.A{"a"}}}}}},
		{"contains escapes", protocol.Cond("name", "contains", "a.b*"), bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: `a\.b\*`}, {Key: "$options", Value: "i"}}}}},
		{"starts", protocol.Cond("name", "starts", "To("), bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: `^To\(`}, {Key: "$options", Value: "i"}}}}},
		{"exists", protocol.Cond("x", "exists", false), bson.D{{Key: "x", Value: bson.D{{Key: "$exists", Value: false}}}}},
		{"id hex", protocol.Cond("_id", "eq", "66d0a1b2c3d4e5f6a7b8c9d0"), bson.D{{Key: "_id", Value: bson.D{{Key: "$eq", Value: oid}}}}},
		{"id in hex", protocol.Cond("_id", "in", []any{"66d0a1b2c3d4e5f6a7b8c9d0", "plain"}), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{oid, "plain"}}}}}},
		{"id not hex", protocol.Cond("_id", "eq", "zz"), bson.D{{Key: "_id", Value: bson.D{{Key: "$eq", Value: "zz"}}}}},
		{"other field hex stays", protocol.Cond("ref", "eq", "66d0a1b2c3d4e5f6a7b8c9d0"), bson.D{{Key: "ref", Value: bson.D{{Key: "$eq", Value: "66d0a1b2c3d4e5f6a7b8c9d0"}}}}},
		{"and or not", protocol.And(protocol.Cond("a", "eq", int64(1)), protocol.Or(protocol.Cond("b", "eq", int64(2)), protocol.Not(protocol.Cond("c", "eq", int64(3))))),
			bson.D{{Key: "$and", Value: bson.A{
				bson.D{{Key: "a", Value: bson.D{{Key: "$eq", Value: int64(1)}}}},
				bson.D{{Key: "$or", Value: bson.A{
					bson.D{{Key: "b", Value: bson.D{{Key: "$eq", Value: int64(2)}}}},
					bson.D{{Key: "$nor", Value: bson.A{bson.D{{Key: "c", Value: bson.D{{Key: "$eq", Value: int64(3)}}}}}}},
				}}},
			}}}},
	}
	for _, c := range cases {
		got, err := Compile(c.f)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %#v\n want %#v", c.name, got, c.want)
		}
	}
	if _, err := Compile(&protocol.Filter{Field: "a", Op: "regex", Value: "x"}); db.CodeOf(err) != protocol.ErrInvalid {
		t.Errorf("unknown op should be invalid: %v", err)
	}
}

func TestProjectionAndSort(t *testing.T) {
	p := projection([]string{"charinfo.firstname", "charinfo", "money.bank", "inventory[].count", "money.bank"})
	want := bson.D{{Key: "charinfo", Value: 1}, {Key: "money", Value: 1}, {Key: "inventory", Value: 1}, {Key: "_id", Value: 0}}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("projection:\n got  %v\n want %v", p, want)
	}
	p = projection([]string{"_id", "name"})
	if !reflect.DeepEqual(p, bson.D{{Key: "_id", Value: 1}, {Key: "name", Value: 1}}) {
		t.Errorf("projection with id: %v", p)
	}
	s := sortDoc([]protocol.SortSpec{{Field: "money.bank", Dir: "desc"}, {Field: "items[].n", Dir: "asc"}})
	if !reflect.DeepEqual(s, bson.D{{Key: "money.bank", Value: -1}, {Key: "items.n", Value: 1}}) {
		t.Errorf("sort: %v", s)
	}
}

func TestNormalize(t *testing.T) {
	oid, _ := bson.ObjectIDFromHex("66d0a1b2c3d4e5f6a7b8c9d0")
	when := time.Date(2026, 9, 1, 18, 20, 11, 0, time.UTC)
	doc := bson.D{
		{Key: "_id", Value: oid},
		{Key: "n", Value: int32(3)},
		{Key: "when", Value: bson.NewDateTimeFromTime(when)},
		{Key: "bin", Value: bson.Binary{Data: []byte{1, 2}}},
		{Key: "dec", Value: mustDecimal("12.5")},
		{Key: "nested", Value: bson.D{{Key: "a", Value: bson.A{int64(1), "x", bson.Null{}}}}},
	}
	m := normalizeDoc(doc)
	if m["_id"] != "66d0a1b2c3d4e5f6a7b8c9d0" || m["n"] != int64(3) || m["dec"] != 12.5 {
		t.Errorf("scalars: %#v", m)
	}
	if got, ok := m["when"].(time.Time); !ok || !got.Equal(when) {
		t.Errorf("date: %#v", m["when"])
	}
	if got, ok := m["bin"].([]byte); !ok || len(got) != 2 {
		t.Errorf("binary: %#v", m["bin"])
	}
	inner := m["nested"].(map[string]any)["a"].([]any)
	if inner[0] != int64(1) || inner[1] != "x" || inner[2] != nil {
		t.Errorf("array: %#v", inner)
	}
	row := db.ProjectRow(m, []string{"_id", "nested.a", "nested.a[]", "missing.x", "when"})
	if row["_id"] != "66d0a1b2c3d4e5f6a7b8c9d0" || row["nested.a"] != `[1,"x",null]` || row["nested.a[]"] != `[1,"x",null]` || row["missing.x"] != nil || row["when"] != "2026-09-01T18:20:11Z" {
		t.Errorf("row: %#v", row)
	}
}

func mustDecimal(s string) bson.Decimal128 {
	d, err := bson.ParseDecimal128(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestDatabaseFromURI(t *testing.T) {
	cases := map[string]string{
		"mongodb://user:p%40ss@127.0.0.1:27017/qbcore?authSource=admin": "qbcore",
		"mongodb://h1:27017,h2:27017/rs?replicaSet=x":                   "rs",
		"mongodb+srv://u:p@cluster.example.net/app":                     "app",
		"mongodb://localhost:27017":                                     "",
		"mongodb://localhost:27017/":                                    "",
		"mongodb://u:p@localhost/my%20db":                               "my db",
	}
	for uri, want := range cases {
		if got := DatabaseFromURI(uri); got != want {
			t.Errorf("DatabaseFromURI(%q) = %q, want %q", uri, got, want)
		}
	}
	host, name := Describe("mongodb://user:secret@h1:1,h2:2/db?x=y")
	if host != "h1:1,h2:2" || name != "db" {
		t.Errorf("describe: %q %q", host, name)
	}
}
