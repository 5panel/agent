package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// roundTrip encodes v, decodes the frame through Decode and re-encodes the
// result; both encodings must be byte-identical.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	first, err := Encode(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(first)
	if err != nil {
		t.Fatalf("decode %s: %v", first, err)
	}
	second, err := Encode(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var a, b any
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip changed the frame:\n %s\n %s", first, second)
	}
	return decoded
}

func TestRoundTripEveryMessage(t *testing.T) {
	limit, skip, sample, examples := 200, 0, 300, 3
	group := "job.name"
	msgs := []any{
		Hello{Type: TypeHello, Protocol: 1, Token: "fp_live_x", Engine: "mysql", Database: "qbcore",
			Agent: AgentInfo{Version: "1.0.0", OS: "linux", Arch: "amd64", Hostname: "srv-1"}, Capabilities: []string{"list", "find"}},
		Welcome{Type: TypeWelcome, ConnectionID: "66d", Name: "Main server", PingIntervalMs: 30000, MaxRows: 500},
		Reject{Type: TypeReject, Reason: RejectBadToken},
		Ping{Type: TypePing, T: 1725800000000},
		Pong{Type: TypePong, T: 1725800000000},
		Bye{Type: TypeBye, Reason: "shutdown"},
		ErrorMsg{Type: TypeError, Message: "unparseable frame"},
		&QueryReq{Type: TypeQueryReq, QueryID: "q1", Op: OpList, TimeoutMs: 5000},
		&QueryReq{Type: TypeQueryReq, QueryID: "q2", Op: OpProfile, Collection: "players", Sample: &sample, Examples: &examples, TimeoutMs: 10000},
		&QueryReq{Type: TypeQueryReq, QueryID: "q3", Op: OpFind, Collection: "players",
			Fields: []string{"citizenid", "charinfo.firstname", "money.bank"},
			Filter: And(Cond("money.bank", "gte", int64(50000)), Or(Cond("job.name", "eq", "police"), Not(Cond("name", "exists", false)))),
			Sort:   []SortSpec{{Field: "money.bank", Dir: "desc"}}, Limit: &limit, Skip: &skip, TimeoutMs: 3000},
		&QueryReq{Type: TypeQueryReq, QueryID: "q4", Op: OpCount, Collection: "players", Filter: Cond("bank", "in", []any{int64(1), "a", nil, true}), TimeoutMs: 3000},
		&QueryReq{Type: TypeQueryReq, QueryID: "q5", Op: OpAggregate, Collection: "players", GroupBy: &group,
			Metrics: []Metric{{Fn: "count"}, {Fn: "sum", Field: "money.bank", As: "bank_total"}}, Limit: &limit, TimeoutMs: 3000},
		&QueryReq{Type: TypeQueryReq, QueryID: "q6", Op: OpWrite, Collection: "bans", Action: ActionInsert,
			Values: map[string]any{"license": "x", "reason": "RDM", "expire": int64(0)}, TimeoutMs: 3000},
		NewError("q1", ErrTimeout, "query exceeded 3000 ms"),
		NewList("q1", []CollectionInfo{{Name: "players", Count: 18422}}),
		NewProfile("q2", &CollectionProfile{Collection: "players", Sampled: 300, Count: 18422,
			Fields: []FieldProfile{{Path: "citizenid", Types: map[string]int{"string": 300}, Examples: []any{"ABC"}, Unique: true}}}),
		NewRows("q3", []Row{{"citizenid": "ABC", "money.bank": float64(12)}}, boolPtr(false)),
		NewRows("q5", []Row{{"group": "police", "count": float64(41)}}, nil),
		NewCount("q4", 812),
		NewAffected("q6", 1),
	}
	for _, m := range msgs {
		roundTrip(t, m)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestResultShapes(t *testing.T) {
	cases := map[string]*QueryRes{
		`{"type":"query_res","query_id":"e","status":"error","execution_time_ms":0,"error":{"code":"invalid","message":"m"}}`: NewError("e", ErrInvalid, "m"),
		`{"type":"query_res","query_id":"l","status":"ok","execution_time_ms":0,"collections":[]}`:                            NewList("l", nil),
		`{"type":"query_res","query_id":"r","status":"ok","execution_time_ms":0,"rows":[],"truncated":false}`:                 NewRows("r", nil, boolPtr(false)),
		`{"type":"query_res","query_id":"a","status":"ok","execution_time_ms":0,"rows":[]}`:                                   NewRows("a", nil, nil),
		`{"type":"query_res","query_id":"c","status":"ok","execution_time_ms":0,"count":0}`:                                   NewCount("c", 0),
		`{"type":"query_res","query_id":"w","status":"ok","execution_time_ms":0,"affected":0}`:                                NewAffected("w", 0),
	}
	for want, res := range cases {
		got, _ := Encode(res)
		var a, b any
		_ = json.Unmarshal([]byte(want), &a)
		_ = json.Unmarshal(got, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("want %s\n got %s", want, got)
		}
	}
}

func TestDecodeRequestNumbers(t *testing.T) {
	m, err := Decode([]byte(`{"type":"query_req","query_id":"q","op":"count","collection":"p","timeout_ms":3000,
		"filter":{"and":[{"field":"a","op":"eq","value":12},{"field":"b","op":"gt","value":1.5},{"field":"c","op":"in","value":[1,"x",null]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	q := m.(*QueryReq)
	if p := q.Validate(); p != "" {
		t.Fatalf("unexpected problem %q", p)
	}
	if v, ok := q.Filter.And[0].Value.(int64); !ok || v != 12 {
		t.Errorf("integral number should be int64, got %T %v", q.Filter.And[0].Value, q.Filter.And[0].Value)
	}
	if v, ok := q.Filter.And[1].Value.(float64); !ok || v != 1.5 {
		t.Errorf("fraction should be float64, got %T", q.Filter.And[1].Value)
	}
	list := q.Filter.And[2].Value.([]any)
	if _, ok := list[0].(int64); !ok || list[1] != "x" || list[2] != nil {
		t.Errorf("list values wrong: %#v", list)
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, err := Decode([]byte(`not json`)); err == nil {
		t.Error("garbage should fail")
	}
	if _, err := Decode([]byte(`{"foo":1}`)); err != ErrNoType {
		t.Errorf("missing type: %v", err)
	}
	_, err := Decode([]byte(`{"type":"dance"}`))
	var ut *UnknownTypeError
	if err == nil || !strings.Contains(err.Error(), "dance") {
		t.Errorf("unknown type: %v", err)
	}
	if !errorAs(err, &ut) {
		t.Errorf("unknown type should be UnknownTypeError, got %T", err)
	}
	// A query_req with a wrong field type keeps its id and reports invalid.
	m, err := Decode([]byte(`{"type":"query_req","query_id":"q9","op":"profile","collection":"p","sample":"lots"}`))
	if err != nil {
		t.Fatal(err)
	}
	q := m.(*QueryReq)
	if q.QueryID != "q9" || !strings.HasPrefix(q.Validate(), "malformed request") {
		t.Errorf("malformed request: id=%q problem=%q", q.QueryID, q.Validate())
	}
}

func errorAs(err error, target **UnknownTypeError) bool {
	u, ok := err.(*UnknownTypeError)
	if ok {
		*target = u
	}
	return ok
}

func TestFilterValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string // substring of the problem, "" for valid
	}{
		{"condition", `{"field":"bank","op":"gte","value":5}`, ""},
		{"null value", `{"field":"bank","op":"eq","value":null}`, ""},
		{"and", `{"and":[{"field":"a","op":"eq","value":1},{"field":"b","op":"ne","value":"x"}]}`, ""},
		{"not", `{"not":{"field":"a","op":"exists","value":true}}`, ""},
		{"in list", `{"field":"a","op":"in","value":[1,"b",null]}`, ""},
		{"array", `[1]`, "must be an object"},
		{"null", `null`, "must be an object"},
		{"object value", `{"field":"a","op":"eq","value":{"$ne":null}}`, "must be text, a number"},
		{"array value for eq", `{"field":"a","op":"eq","value":[1]}`, "must be text, a number"},
		{"missing value", `{"field":"a","op":"eq"}`, "needs a value"},
		{"bad op", `{"field":"a","op":"regex","value":"x"}`, "unknown operator"},
		{"bad path", `{"field":"a..b","op":"eq","value":1}`, "field path"},
		{"dollar path", `{"field":"$where","op":"eq","value":1}`, "field path"},
		{"and with extra key", `{"and":[{"field":"a","op":"eq","value":1}],"field":"x"}`, "only key"},
		{"empty and", `{"and":[]}`, "non-empty list"},
		{"or not list", `{"or":{"field":"a","op":"eq","value":1}}`, "non-empty list"},
		{"in not list", `{"field":"a","op":"in","value":"x"}`, "needs a list"},
		{"in object element", `{"field":"a","op":"in","value":[{"$gt":1}]}`, "needs a list"},
		{"exists text", `{"field":"a","op":"exists","value":"yes"}`, "true or false"},
		{"contains number", `{"field":"a","op":"contains","value":5}`, "needs text"},
		{"too deep", strings.Repeat(`{"not":`, 7) + `{"field":"a","op":"eq","value":1}` + strings.Repeat("}", 7), "too deep"},
		{"too many", `{"and":[` + strings.TrimSuffix(strings.Repeat(`{"field":"a","op":"eq","value":1},`, 65), ",") + `]}`, "too many"},
	}
	for _, c := range cases {
		var f Filter
		if err := json.Unmarshal([]byte(c.json), &f); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		got := ValidateFilter(&f)
		if c.want == "" && got != "" {
			t.Errorf("%s: unexpected problem %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want problem containing %q, got %q", c.name, c.want, got)
		}
	}
}

func TestPathsAndIdentifiers(t *testing.T) {
	good := []string{"bank", "charinfo.firstname", "money.bank", "inventory[].count", "a-b.c_d", "_x", "a.b.c.d.e.f.g.h", "items[]"}
	bad := []string{"", ".a", "a.", "a..b", "1a", "a b", "a;b", "a[0]", "a[]b", "$where", "a.b.c.d.e.f.g.h.i", strings.Repeat("a", 65), "a.`b`"}
	for _, p := range good {
		if !IsPath(p) {
			t.Errorf("IsPath(%q) should be true", p)
		}
	}
	for _, p := range bad {
		if IsPath(p) {
			t.Errorf("IsPath(%q) should be false", p)
		}
	}
	for _, id := range []string{"players", "_t", "A1", strings.Repeat("a", 64)} {
		if !IsIdentifier(id) {
			t.Errorf("IsIdentifier(%q) should be true", id)
		}
	}
	for _, id := range []string{"", "1a", "a-b", "a b", "a`b", "players; DROP TABLE x", "a.b", strings.Repeat("a", 65), "x\x00"} {
		if IsIdentifier(id) {
			t.Errorf("IsIdentifier(%q) should be false", id)
		}
	}
	for _, c := range []string{"players", "player-vehicles", "Ünïcode", "with space?no"} {
		want := !strings.Contains(c, " ")
		if IsCollectionName(c) != want {
			t.Errorf("IsCollectionName(%q) = %v", c, !want)
		}
	}
	for _, c := range []string{"", "$cmd", "a.b", "system.users", "x\x00y", strings.Repeat("a", 121)} {
		if IsCollectionName(c) {
			t.Errorf("IsCollectionName(%q) should be false", c)
		}
	}
	segs := ParsePath("inventory[].items[].count")
	if len(segs) != 3 || !segs[0].Array || segs[0].Name != "inventory" || !segs[1].Array || segs[2].Array || segs[2].Name != "count" {
		t.Errorf("ParsePath: %#v", segs)
	}
	if !IsToken("fp_live_"+strings.Repeat("A", 43)) || IsToken("fp_live_short") {
		t.Error("IsToken")
	}
}

func TestMetricKey(t *testing.T) {
	cases := map[Metric]string{
		{Fn: "count"}:                             "count",
		{Fn: "sum", Field: "money.bank"}:          "sum_money_bank",
		{Fn: "avg", Field: "a-b"}:                 "avg_a_b",
		{Fn: "max", Field: "inventory[].count"}:   "max_inventory_count",
		{Fn: "sum", Field: "x", As: "bank_total"}: "bank_total",
	}
	for m, want := range cases {
		if got := MetricKey(m); got != want {
			t.Errorf("MetricKey(%v) = %q, want %q", m, got, want)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	n := func(i int) *int { return &i }
	s := func(x string) *string { return &x }
	base := func(op string) QueryReq {
		return QueryReq{Type: TypeQueryReq, QueryID: "q", Op: op, Collection: "players", TimeoutMs: 3000}
	}
	find := func(mod func(q *QueryReq)) QueryReq {
		q := base(OpFind)
		q.Fields = []string{"a"}
		mod(&q)
		return q
	}
	agg := func(mod func(q *QueryReq)) QueryReq {
		q := base(OpAggregate)
		q.Metrics = []Metric{{Fn: "count"}}
		mod(&q)
		return q
	}
	cases := []struct {
		name string
		req  QueryReq
		want string
	}{
		{"list", QueryReq{Type: TypeQueryReq, QueryID: "q", Op: OpList}, ""},
		{"list no id", QueryReq{Type: TypeQueryReq, Op: OpList}, "query_id"},
		{"timeout low", QueryReq{Type: TypeQueryReq, QueryID: "q", Op: OpList, TimeoutMs: 50}, "timeout"},
		{"timeout high", QueryReq{Type: TypeQueryReq, QueryID: "q", Op: OpList, TimeoutMs: 20000}, "timeout"},
		{"bad collection", func() QueryReq { q := base(OpCount); q.Collection = "system.x"; return q }(), "collection"},
		{"profile ok", base(OpProfile), ""},
		{"profile sample", func() QueryReq { q := base(OpProfile); q.Sample = n(5000); return q }(), "sample"},
		{"profile examples", func() QueryReq { q := base(OpProfile); q.Examples = n(11); return q }(), "examples"},
		{"find ok", find(func(q *QueryReq) {}), ""},
		{"find no fields", find(func(q *QueryReq) { q.Fields = nil }), "fields"},
		{"find bad field", find(func(q *QueryReq) { q.Fields = []string{"a", "b c"} }), "fields"},
		{"find 65 fields", find(func(q *QueryReq) { q.Fields = make65() }), "fields"},
		{"find sort dir", find(func(q *QueryReq) { q.Sort = []SortSpec{{Field: "a", Dir: "up"}} }), "sort"},
		{"find sort 5", find(func(q *QueryReq) {
			q.Sort = []SortSpec{{"a", "asc"}, {"b", "asc"}, {"c", "asc"}, {"d", "asc"}, {"e", "asc"}}
		}), "sort"},
		{"find limit", find(func(q *QueryReq) { q.Limit = n(501) }), "limit"},
		{"find skip", find(func(q *QueryReq) { q.Skip = n(-1) }), "skip"},
		{"find filter", find(func(q *QueryReq) { q.Filter = Cond("a", "eq", map[string]any{}) }), "must be text"},
		{"count ok", base(OpCount), ""},
		{"agg ok", agg(func(q *QueryReq) { q.GroupBy = s("job.name") }), ""},
		{"agg bad group", agg(func(q *QueryReq) { q.GroupBy = s("1x") }), "group_by"},
		{"agg no metrics", agg(func(q *QueryReq) { q.Metrics = nil }), "metrics"},
		{"agg bad fn", agg(func(q *QueryReq) { q.Metrics = []Metric{{Fn: "median", Field: "a"}} }), "bad metric"},
		{"agg needs field", agg(func(q *QueryReq) { q.Metrics = []Metric{{Fn: "sum"}} }), "needs a field"},
		{"agg bad as", agg(func(q *QueryReq) { q.Metrics = []Metric{{Fn: "sum", Field: "a", As: "a b"}} }), "metric name"},
		{"agg as group", agg(func(q *QueryReq) { q.Metrics = []Metric{{Fn: "sum", Field: "a", As: "group"}} }), "duplicate"},
		{"agg dup", agg(func(q *QueryReq) { q.Metrics = []Metric{{Fn: "count"}, {Fn: "count"}} }), "duplicate"},
		{"agg limit", agg(func(q *QueryReq) { q.Limit = n(0) }), "limit"},
		{"write insert", func() QueryReq {
			q := base(OpWrite)
			q.Action = ActionInsert
			q.Values = map[string]any{"a": int64(1)}
			return q
		}(), ""},
		{"write insert empty", func() QueryReq { q := base(OpWrite); q.Action = ActionInsert; return q }(), "insert needs values"},
		{"write insert object", func() QueryReq {
			q := base(OpWrite)
			q.Action = ActionInsert
			q.Values = map[string]any{"a": map[string]any{}}
			return q
		}(), "insert needs values"},
		{"write update", func() QueryReq {
			q := base(OpWrite)
			q.Action = ActionUpdate
			q.Set = map[string]any{"a": "b"}
			q.Filter = Cond("id", "eq", int64(1))
			return q
		}(), ""},
		{"write bad action", func() QueryReq { q := base(OpWrite); q.Action = "truncate"; return q }(), "bad action"},
		{"write limit", func() QueryReq {
			q := base(OpWrite)
			q.Action = ActionDelete
			q.Filter = Cond("id", "eq", int64(1))
			q.Limit = n(101)
			return q
		}(), "limit"},
		{"unknown op", base("explain"), "unknown operation"},
	}
	for _, c := range cases {
		got := c.req.Validate()
		if c.want == "" && got != "" {
			t.Errorf("%s: unexpected problem %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q, got %q", c.name, c.want, got)
		}
	}
}

func make65() []string {
	out := make([]string, 65)
	for i := range out {
		out[i] = "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return out
}
