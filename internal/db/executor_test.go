package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/5panel/agent/internal/protocol"
)

// fakeStore records the queries it receives and answers from canned data.
type fakeStore struct {
	docs     []map[string]any
	find     FindQuery
	agg      AggregateQuery
	write    WriteQuery
	delay    time.Duration
	failWith error
}

func (f *fakeStore) Engine() string             { return "mysql" }
func (f *fakeStore) Name() string               { return "test" }
func (f *fakeStore) Ping(context.Context) error { return nil }
func (f *fakeStore) Close(context.Context) error {
	return nil
}
func (f *fakeStore) List(context.Context) ([]protocol.CollectionInfo, error) {
	return []protocol.CollectionInfo{{Name: "players", Count: 2}}, f.failWith
}
func (f *fakeStore) Sample(context.Context, string, int) ([]map[string]any, int64, error) {
	return f.docs, int64(len(f.docs)), nil
}
func (f *fakeStore) Find(ctx context.Context, q FindQuery) ([]protocol.Row, error) {
	f.find = q
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failWith != nil {
		return nil, f.failWith
	}
	var rows []protocol.Row
	for i, d := range f.docs {
		if i >= q.Limit {
			break
		}
		rows = append(rows, ProjectRow(d, q.Fields))
	}
	return rows, nil
}
func (f *fakeStore) Count(context.Context, string, *protocol.Filter) (int64, error) {
	return int64(len(f.docs)), nil
}
func (f *fakeStore) Aggregate(_ context.Context, q AggregateQuery) ([]protocol.Row, error) {
	f.agg = q
	return []protocol.Row{{"group": nil, "count": int64(len(f.docs))}}, nil
}
func (f *fakeStore) Write(_ context.Context, q WriteQuery) (int64, error) {
	f.write = q
	return 1, nil
}

func decode(t *testing.T, data []byte) *protocol.QueryRes {
	t.Helper()
	var res protocol.QueryRes
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("bad result %s: %v", data, err)
	}
	return &res
}

func newExecutor(store *fakeStore) *Executor {
	return &Executor{Store: store, Limits: Limits{ReadOnly: true, QueryTimeout: 3 * time.Second, MaxRows: 500, MaxResultBytes: 4_000_000}}
}

func manyDocs(n int) []map[string]any {
	docs := make([]map[string]any, n)
	for i := range docs {
		docs[i] = map[string]any{"id": int64(i), "charinfo": `{"firstname":"Tommy"}`}
	}
	return docs
}

func TestFindCapsAndTruncation(t *testing.T) {
	store := &fakeStore{docs: manyDocs(1000)}
	e := newExecutor(store)
	limit := 200
	req := &protocol.QueryReq{Type: "query_req", QueryID: "q", Op: "find", Collection: "players", Fields: []string{"id", "charinfo.firstname"}, Limit: &limit}

	res := decode(t, e.Execute(context.Background(), req, 500))
	if res.Status != "ok" || len(res.Rows) != 200 || res.Truncated == nil || !*res.Truncated {
		t.Fatalf("find: status=%s rows=%d truncated=%v", res.Status, len(res.Rows), res.Truncated)
	}
	if store.find.Limit != 201 {
		t.Errorf("store should be asked for limit+1, got %d", store.find.Limit)
	}
	if res.Rows[0]["charinfo.firstname"] != "Tommy" {
		t.Errorf("nested projection: %v", res.Rows[0])
	}

	// The gateway's max_rows lowers the cap; so does security.max_rows_limit.
	res = decode(t, e.Execute(context.Background(), req, 50))
	if len(res.Rows) != 50 {
		t.Errorf("gateway cap: %d", len(res.Rows))
	}
	e.Limits.MaxRows = 10
	res = decode(t, e.Execute(context.Background(), req, 500))
	if len(res.Rows) != 10 {
		t.Errorf("config cap: %d", len(res.Rows))
	}

	// Default limit is 100 and a result under the cap is not truncated.
	e.Limits.MaxRows = 500
	store.docs = manyDocs(5)
	req.Limit = nil
	res = decode(t, e.Execute(context.Background(), req, 500))
	if len(res.Rows) != 5 || *res.Truncated || store.find.Limit != 101 {
		t.Errorf("default limit: rows=%d truncated=%v asked=%d", len(res.Rows), *res.Truncated, store.find.Limit)
	}
}

func TestInvalidBeforeStore(t *testing.T) {
	store := &fakeStore{docs: manyDocs(1)}
	e := newExecutor(store)
	req := &protocol.QueryReq{Type: "query_req", QueryID: "q", Op: "find", Collection: "players; DROP", Fields: []string{"id"}}
	res := decode(t, e.Execute(context.Background(), req, 0))
	if res.Status != "error" || res.Error.Code != "invalid" || res.QueryID != "q" {
		t.Errorf("invalid: %+v", res)
	}
	if store.find.Collection != "" {
		t.Error("store must not be called for an invalid request")
	}
	res = decode(t, e.Execute(context.Background(), &protocol.QueryReq{Type: "query_req", QueryID: "u", Op: "explain", Collection: "x"}, 0))
	if res.Error == nil || res.Error.Code != "unsupported" {
		t.Errorf("unknown op: %+v", res)
	}
}

func TestTimeoutAndDBErrors(t *testing.T) {
	store := &fakeStore{docs: manyDocs(1), delay: time.Second}
	e := newExecutor(store)
	e.Limits.QueryTimeout = 50 * time.Millisecond
	req := &protocol.QueryReq{Type: "query_req", QueryID: "q", Op: "find", Collection: "players", Fields: []string{"id"}, TimeoutMs: 5000}
	res := decode(t, e.Execute(context.Background(), req, 0))
	if res.Error == nil || res.Error.Code != "timeout" || !strings.Contains(res.Error.Message, "50 ms") {
		t.Errorf("timeout: %+v", res)
	}

	store.delay = 0
	store.failWith = errors.New("Table 'x.players' doesn't exist")
	res = decode(t, e.Execute(context.Background(), req, 0))
	if res.Error == nil || res.Error.Code != "db" || !strings.Contains(res.Error.Message, "doesn't exist") {
		t.Errorf("db error: %+v", res)
	}
	store.failWith = Unsupported("no arrays here")
	res = decode(t, e.Execute(context.Background(), req, 0))
	if res.Error == nil || res.Error.Code != "unsupported" {
		t.Errorf("unsupported: %+v", res)
	}
}

func TestMaxResultBytes(t *testing.T) {
	store := &fakeStore{docs: manyDocs(100)}
	e := newExecutor(store)
	e.Limits.MaxResultBytes = 500
	req := &protocol.QueryReq{Type: "query_req", QueryID: "q", Op: "find", Collection: "players", Fields: []string{"id", "charinfo"}}
	res := decode(t, e.Execute(context.Background(), req, 0))
	if res.Error == nil || res.Error.Code != "invalid" || !strings.Contains(res.Error.Message, "max_result_bytes") {
		t.Errorf("oversized result: %+v", res)
	}
}

func TestWriteGuards(t *testing.T) {
	store := &fakeStore{}
	e := newExecutor(store)
	insert := &protocol.QueryReq{Type: "query_req", QueryID: "w", Op: "write", Collection: "bans", Action: "insert", Values: map[string]any{"reason": "RDM"}}
	res := decode(t, e.Execute(context.Background(), insert, 0))
	if res.Error == nil || res.Error.Code != "refused" || !strings.Contains(res.Error.Message, "read-only") {
		t.Errorf("read-only: %+v", res)
	}
	if got := e.Capabilities(); strings.Contains(strings.Join(got, ","), "write") {
		t.Errorf("read-only agent must not announce write: %v", got)
	}

	e.Limits.ReadOnly = false
	res = decode(t, e.Execute(context.Background(), insert, 0))
	if res.Error == nil || res.Error.Code != "refused" || !strings.Contains(res.Error.Message, "writes.allow") {
		t.Errorf("allowlist: %+v", res)
	}
	if strings.Contains(strings.Join(e.Capabilities(), ","), "write") {
		t.Error("empty allowlist must not announce write")
	}

	e.Limits.WriteAllow = []string{"bans"}
	if !strings.Contains(strings.Join(e.Capabilities(), ","), "write") {
		t.Error("write should be announced now")
	}
	res = decode(t, e.Execute(context.Background(), insert, 0))
	if res.Status != "ok" || res.Affected == nil || *res.Affected != 1 {
		t.Errorf("insert: %+v", res)
	}

	del := &protocol.QueryReq{Type: "query_req", QueryID: "d", Op: "write", Collection: "bans", Action: "delete"}
	res = decode(t, e.Execute(context.Background(), del, 0))
	if res.Error == nil || res.Error.Code != "refused" || !strings.Contains(res.Error.Message, "filter") {
		t.Errorf("delete without filter: %+v", res)
	}
	del.Filter = protocol.Cond("license", "eq", "abc")
	limit := 500
	del.Limit = &limit
	res = decode(t, e.Execute(context.Background(), del, 0))
	if res.Error == nil || res.Error.Code != "invalid" {
		t.Errorf("write limit over 100 is invalid: %+v", res)
	}
	del.Limit = nil
	res = decode(t, e.Execute(context.Background(), del, 0))
	if res.Status != "ok" || store.write.Limit != 1 {
		t.Errorf("delete default limit 1: %+v limit=%d", res, store.write.Limit)
	}
}

func TestProfileAndAggregateDefaults(t *testing.T) {
	store := &fakeStore{docs: manyDocs(3)}
	e := newExecutor(store)
	res := decode(t, e.Execute(context.Background(), &protocol.QueryReq{Type: "query_req", QueryID: "p", Op: "profile", Collection: "players"}, 0))
	if res.Status != "ok" || res.Profile == nil || res.Profile.Sampled != 3 || res.Profile.Count != 3 || len(res.Profile.Fields) != 3 {
		t.Errorf("profile: %+v", res.Profile)
	}
	res = decode(t, e.Execute(context.Background(), &protocol.QueryReq{Type: "query_req", QueryID: "a", Op: "aggregate", Collection: "players",
		Metrics: []protocol.Metric{{Fn: "count"}, {Fn: "sum", Field: "money.bank"}}}, 0))
	if res.Status != "ok" || len(res.Rows) != 1 || res.Truncated != nil {
		t.Errorf("aggregate: %+v", res)
	}
	if store.agg.Limit != 50 || store.agg.Metrics[1].As != "sum_money_bank" || store.agg.Metrics[0].As != "count" {
		t.Errorf("aggregate defaults: %+v", store.agg)
	}
}

func TestExtractAndScalars(t *testing.T) {
	doc := map[string]any{
		"charinfo":  `{"firstname":"Tommy","addr":{"city":"LS"}}`,
		"inventory": `[{"name":"water","count":3},{"name":"bread","count":1}]`,
		"money":     map[string]any{"bank": int64(5)},
		"when":      time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC),
		"bin":       []byte("hi"),
		"nan":       float64(1) / 3,
	}
	row := ProjectRow(doc, []string{"charinfo.firstname", "charinfo.addr.city", "charinfo", "inventory[].count", "inventory.count", "inventory[]", "money", "money.bank", "money.none", "when", "bin", "nope", "charinfo.firstname[]"})
	want := protocol.Row{
		"charinfo.firstname":   "Tommy",
		"charinfo.addr.city":   "LS",
		"charinfo":             `{"firstname":"Tommy","addr":{"city":"LS"}}`,
		"inventory[].count":    "[3,1]",
		"inventory.count":      "[3,1]",
		"inventory[]":          `[{"name":"water","count":3},{"name":"bread","count":1}]`, // verbatim text
		"money":                `{"bank":5}`,
		"money.bank":           int64(5),
		"money.none":           nil,
		"when":                 "2026-01-02T03:04:05.6Z",
		"bin":                  "aGk=",
		"nope":                 nil,
		"charinfo.firstname[]": nil,
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s: got %#v want %#v", k, row[k], v)
		}
	}
}
