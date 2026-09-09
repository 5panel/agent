package profile

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/5panel/agent/internal/protocol"
)

func byPath(fields []protocol.FieldProfile) map[string]protocol.FieldProfile {
	m := map[string]protocol.FieldProfile{}
	for _, f := range fields {
		m[f.Path] = f
	}
	return m
}

func TestProfileShapes(t *testing.T) {
	when := time.Date(2026, 9, 1, 18, 20, 11, 0, time.UTC)
	docs := []map[string]any{
		{"citizenid": "ABC12345", "charinfo": `{"firstname":"Tommy","lastname":"Vercetti","age":31}`, "money": map[string]any{"bank": int64(1250000)},
			"inventory": []any{map[string]any{"name": "water", "count": int64(3)}, map[string]any{"name": "bread", "count": int64(1)}},
			"tags":      []any{"a", "b"}, "last_updated": when, "blob": []byte{1, 2}, "deep": map[string]any{"l2": map[string]any{"l3": map[string]any{"l4": 1}}}},
		{"citizenid": "QWE98765", "charinfo": `{"firstname":"Carl"}`, "money": map[string]any{"bank": nil}, "inventory": []any{},
			"tags": nil, "last_updated": when, "flag": true, "1": "slot key ignored"},
		{"citizenid": "ZXC11223", "charinfo": "not json {", "money": map[string]any{"bank": 12.5}, "flag": false},
	}
	fields := byPath(Build(docs, Options{Examples: 3}))

	want := map[string]map[string]int{
		"citizenid":          {"string": 3},
		"charinfo":           {"json": 2, "string": 1},
		"charinfo.firstname": {"string": 2},
		"charinfo.lastname":  {"string": 1},
		"charinfo.age":       {"number": 1},
		"money":              {"json": 3},
		"money.bank":         {"number": 2, "null": 1},
		"inventory":          {"json": 2},
		"inventory[]":        {"json": 2},
		"inventory[].name":   {"string": 2},
		"inventory[].count":  {"number": 2},
		"tags":               {"json": 1, "null": 1},
		"tags[]":             {"string": 2},
		"last_updated":       {"date": 2},
		"blob":               {"binary": 1},
		"flag":               {"boolean": 2},
		"deep":               {"json": 1},
		"deep.l2":            {"json": 1},
		"deep.l2.l3":         {"json": 1},
	}
	for path, types := range want {
		f, ok := fields[path]
		if !ok {
			t.Errorf("missing field %s", path)
			continue
		}
		if fmt.Sprint(f.Types) != fmt.Sprint(types) {
			t.Errorf("%s types: got %v want %v", path, f.Types, types)
		}
	}
	if _, ok := fields["deep.l2.l3.l4"]; ok {
		t.Error("depth 4 should not be listed")
	}
	if _, ok := fields["1"]; ok {
		t.Error("non-segment key should not be listed")
	}
	if !fields["citizenid"].Unique {
		t.Error("citizenid should be unique")
	}
	for _, p := range []string{"charinfo", "money.bank", "inventory[].count", "last_updated", "flag", "blob"} {
		if fields[p].Unique {
			t.Errorf("%s should not be unique", p)
		}
	}
	if ex := fields["charinfo.firstname"].Examples; len(ex) != 2 || ex[0] != "Tommy" || ex[1] != "Carl" {
		t.Errorf("firstname examples: %v", ex)
	}
	if ex := fields["money.bank"].Examples; len(ex) != 2 || ex[0] != int64(1250000) || ex[1] != 12.5 {
		t.Errorf("bank examples: %v", ex)
	}
	if ex := fields["last_updated"].Examples; len(ex) != 1 || ex[0] != "2026-09-01T18:20:11Z" {
		t.Errorf("date examples: %v", ex)
	}
	for _, p := range []string{"money", "blob", "inventory", "deep"} {
		if len(fields[p].Examples) != 0 {
			t.Errorf("%s should have no examples: %v", p, fields[p].Examples)
		}
	}
	// A field that is JSON in some rows and plain text in others gives the
	// text values as examples, never the containers.
	if ex := fields["charinfo"].Examples; len(ex) != 1 || ex[0] != "not json {" {
		t.Errorf("mixed charinfo examples: %v", ex)
	}
}

func TestExamplesCappedAndTruncated(t *testing.T) {
	long := strings.Repeat("x", 100)
	veryLong := strings.Repeat("y", 300)
	var docs []map[string]any
	for i := 0; i < 20; i++ {
		docs = append(docs, map[string]any{"name": fmt.Sprintf("n%d", i%5), "long": long, "text": veryLong, "u": fmt.Sprintf("Ünï%d", i)})
	}
	fields := byPath(Build(docs, Options{Examples: 3}))
	if ex := fields["name"].Examples; len(ex) != 3 || ex[0] != "n0" || ex[1] != "n1" || ex[2] != "n2" {
		t.Errorf("capped distinct examples: %v", ex)
	}
	if fields["name"].Unique {
		t.Error("repeated names are not unique")
	}
	if ex := fields["long"].Examples; len(ex) != 1 || len(ex[0].(string)) != 40 {
		t.Errorf("long example should be cut to 40: %v", ex)
	}
	if ex := fields["text"].Examples; len(ex) != 0 {
		t.Errorf("very long text gives no example: %v", ex)
	}
	if fields["text"].Types["string"] != 20 {
		t.Errorf("very long text is still counted: %v", fields["text"].Types)
	}
	if !fields["u"].Unique || len(fields["u"].Examples) != 3 {
		t.Errorf("unique unicode: %+v", fields["u"])
	}
	none := byPath(Build(docs, Options{Examples: 0}))
	if len(none["name"].Examples) != 0 {
		t.Error("examples 0 gives none")
	}
}

func TestUniqueRequiresEveryDoc(t *testing.T) {
	docs := []map[string]any{{"id": int64(1)}, {"id": int64(2)}, {"other": "x"}}
	fields := byPath(Build(docs, Options{Examples: 1}))
	if fields["id"].Unique {
		t.Error("a field missing on some documents is not the unique id")
	}
	docs = []map[string]any{{"id": int64(1)}, {"id": int64(1)}}
	if byPath(Build(docs, Options{}))["id"].Unique {
		t.Error("duplicates are not unique")
	}
}

func TestJSONStringDetection(t *testing.T) {
	if _, ok := ParseJSONText(`{"firstname":"Tommy"}`); !ok {
		t.Error("object text")
	}
	if v, ok := ParseJSONText(` [1, 2] `); !ok || len(v.([]any)) != 2 {
		t.Error("array text with spaces")
	}
	for _, s := range []string{"", "{", "{}x", "12", `"str"`, "true", "[1,", "{a:1}"} {
		if _, ok := ParseJSONText(s); ok {
			t.Errorf("%q should not parse", s)
		}
	}
	if v, _ := ParseJSONText(`{"n":9007199254740993}`); v.(map[string]any)["n"] != int64(9007199254740993) {
		t.Error("big integers keep precision")
	}
}

func TestFieldCap(t *testing.T) {
	doc := map[string]any{}
	for i := 0; i < 50; i++ {
		doc[fmt.Sprintf("f%02d", i)] = i
	}
	fields := Build([]map[string]any{doc}, Options{MaxFields: 10})
	if len(fields) != 10 {
		t.Errorf("field cap: %d", len(fields))
	}
}
