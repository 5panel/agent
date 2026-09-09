package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

// fakeStore answers list only.
type fakeStore struct{}

func (fakeStore) Engine() string              { return "mysql" }
func (fakeStore) Name() string                { return "qbcore" }
func (fakeStore) Ping(context.Context) error  { return nil }
func (fakeStore) Close(context.Context) error { return nil }
func (fakeStore) List(context.Context) ([]protocol.CollectionInfo, error) {
	return []protocol.CollectionInfo{{Name: "players", Count: 18422}, {Name: "player_vehicles", Count: 4021}}, nil
}
func (fakeStore) Sample(context.Context, string, int) ([]map[string]any, int64, error) {
	return nil, 0, nil
}
func (fakeStore) Find(context.Context, db.FindQuery) ([]protocol.Row, error) { return nil, nil }
func (fakeStore) Count(context.Context, string, *protocol.Filter) (int64, error) {
	return 0, nil
}
func (fakeStore) Aggregate(context.Context, db.AggregateQuery) ([]protocol.Row, error) {
	return nil, nil
}
func (fakeStore) Write(context.Context, db.WriteQuery) (int64, error) { return 0, nil }

// gatewayConn is the server side of one accepted socket.
type gatewayConn struct {
	t    *testing.T
	conn *websocket.Conn
}

func (g *gatewayConn) send(v any) {
	g.t.Helper()
	data, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.conn.Write(ctx, websocket.MessageText, data); err != nil {
		g.t.Errorf("server write: %v", err)
	}
}

func (g *gatewayConn) read() any {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := g.conn.Read(ctx)
	if err != nil {
		return err
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		g.t.Fatalf("server decode: %v", err)
	}
	return msg
}

// serve starts a fake gateway; script runs per connection and conns counts
// the connections accepted so far (incremented before script runs).
func serve(t *testing.T, conns *atomic.Int32, script func(g *gatewayConn)) (url string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		conns.Add(1)
		defer c.CloseNow()
		script(&gatewayConn{t: t, conn: c})
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func options(url string) Options {
	exec := &db.Executor{Store: fakeStore{}, Limits: db.Limits{ReadOnly: true, QueryTimeout: time.Second, MaxRows: 500, MaxResultBytes: 1 << 20}}
	return Options{
		URL: url, Token: "fp_live_test", Engine: "mysql", Database: "qbcore",
		Agent:        protocol.AgentInfo{Version: "test", OS: "linux", Arch: "amd64", Hostname: "h"},
		Capabilities: exec.Capabilities(), MaxConcurrent: 2, Handler: exec,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, DialTimeout: 2 * time.Second,
	}
}

func expectHello(t *testing.T, g *gatewayConn) *protocol.Hello {
	t.Helper()
	msg := g.read()
	hello, ok := msg.(*protocol.Hello)
	if !ok {
		t.Fatalf("first frame should be hello, got %T %v", msg, msg)
	}
	if hello.Protocol != 1 || hello.Token != "fp_live_test" || hello.Engine != "mysql" || hello.Database != "qbcore" || hello.Agent.Hostname != "h" {
		t.Errorf("hello: %+v", hello)
	}
	if strings.Contains(strings.Join(hello.Capabilities, ","), "write") {
		t.Errorf("read-only agent announced write: %v", hello.Capabilities)
	}
	return hello
}

func TestSessionPingListReplaced(t *testing.T) {
	done := make(chan struct{})
	var conns atomic.Int32
	url := serve(t, &conns, func(g *gatewayConn) {
		defer close(done)
		expectHello(t, g)
		g.send(protocol.Welcome{Type: "welcome", ConnectionID: "c1", Name: "Main", PingIntervalMs: 60000, MaxRows: 500})

		g.send(protocol.Ping{Type: "ping", T: 1725800000000})
		if pong, ok := g.read().(*protocol.Pong); !ok || pong.T != 1725800000000 {
			t.Errorf("expected pong with same t")
		}

		g.send(map[string]any{"type": "query_req", "query_id": "q1", "op": "list", "timeout_ms": 5000})
		res, ok := g.read().(*protocol.QueryRes)
		if !ok || res.QueryID != "q1" || res.Status != "ok" || len(res.Collections) != 2 || res.Collections[0].Name != "players" {
			t.Errorf("list result: %+v", res)
		}

		// Concurrency: two requests in flight answer in some order, both ok.
		g.send(map[string]any{"type": "query_req", "query_id": "q2", "op": "list", "timeout_ms": 5000})
		g.send(map[string]any{"type": "query_req", "query_id": "q3", "op": "count", "collection": "players", "timeout_ms": 5000})
		ids := map[string]bool{}
		for i := 0; i < 2; i++ {
			if r, ok := g.read().(*protocol.QueryRes); ok && r.Status == "ok" {
				ids[r.QueryID] = true
			}
		}
		if !ids["q2"] || !ids["q3"] {
			t.Errorf("expected q2 and q3, got %v", ids)
		}

		// Validation answers invalid without touching the store.
		g.send(map[string]any{"type": "query_req", "query_id": "q4", "op": "find", "collection": "players", "timeout_ms": 5000})
		if r, ok := g.read().(*protocol.QueryRes); !ok || r.Status != "error" || r.Error.Code != "invalid" {
			t.Errorf("invalid: %+v", r)
		}

		// Unknown types get an error frame and the socket stays open.
		g.send(map[string]any{"type": "dance"})
		if e, ok := g.read().(*protocol.ErrorMsg); !ok || !strings.Contains(e.Message, "dance") {
			t.Errorf("unknown type: %v", e)
		}
		g.send(map[string]any{"type": "query_req", "query_id": "q5", "op": "list", "timeout_ms": 5000})
		if r, ok := g.read().(*protocol.QueryRes); !ok || r.QueryID != "q5" {
			t.Errorf("socket should still work after an unknown frame: %v", r)
		}

		g.send(protocol.Reject{Type: "reject", Reason: "replaced"})
		_ = g.conn.Close(websocket.StatusNormalClosure, "replaced")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Run(ctx, options(url))
	if err != nil {
		t.Fatalf("replaced should return nil, got %v", err)
	}
	<-done
}

func TestBadTokenExits(t *testing.T) {
	var conns atomic.Int32
	url := serve(t, &conns, func(g *gatewayConn) {
		expectHello(t, g)
		g.send(protocol.Reject{Type: "reject", Reason: "bad_token"})
		_ = g.conn.Close(websocket.StatusNormalClosure, "bye")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := Run(ctx, options(url))
	if !errors.Is(err, ErrBadToken) {
		t.Fatalf("want ErrBadToken, got %v", err)
	}
	if conns.Load() != 1 {
		t.Errorf("must not reconnect after bad_token, saw %d connections", conns.Load())
	}
	if time.Since(start) > 2*time.Second {
		t.Error("bad_token should return at once")
	}
}

func TestReconnectsWithBackoffAndHeartbeat(t *testing.T) {
	second := make(chan struct{})
	var conns atomic.Int32
	url := serve(t, &conns, func(g *gatewayConn) {
		expectHello(t, g)
		switch {
		case conns.Load() == 1:
			// Drop the first connection without a word.
			return
		case conns.Load() == 2:
			// Welcome with a tiny ping interval and never answer the agent's
			// pings: the agent must drop after two misses.
			g.send(protocol.Welcome{Type: "welcome", ConnectionID: "c2", Name: "Main", PingIntervalMs: 20, MaxRows: 500})
			pings := 0
			for {
				msg := g.read()
				if _, ok := msg.(*protocol.Ping); ok {
					pings++
					continue
				}
				if _, isErr := msg.(error); isErr {
					if pings < 2 {
						t.Errorf("agent dropped after %d pings", pings)
					}
					return
				}
			}
		default:
			g.send(protocol.Welcome{Type: "welcome", ConnectionID: "c3", Name: "Main", PingIntervalMs: 60000, MaxRows: 500})
			close(second)
			// Hold the connection until the client goes away.
			for {
				if _, isErr := g.read().(error); isErr {
					return
				}
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, options(url)) }()
	select {
	case <-second:
	case <-time.After(8 * time.Second):
		t.Fatal("agent did not reconnect a third time")
	}
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("shutdown should return nil, got %v", err)
	}
	if conns.Load() < 3 {
		t.Errorf("expected at least 3 connections, saw %d", conns.Load())
	}
}

func TestByeOnShutdown(t *testing.T) {
	gotBye := make(chan string, 1)
	var conns atomic.Int32
	url := serve(t, &conns, func(g *gatewayConn) {
		expectHello(t, g)
		g.send(protocol.Welcome{Type: "welcome", ConnectionID: "c", Name: "Main", PingIntervalMs: 60000, MaxRows: 500})
		for {
			msg := g.read()
			if bye, ok := msg.(*protocol.Bye); ok {
				gotBye <- bye.Reason
				return
			}
			if _, isErr := msg.(error); isErr {
				gotBye <- ""
				return
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Run(ctx, options(url)) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case reason := <-gotBye:
		if reason != "shutdown" {
			t.Errorf("expected bye shutdown, got %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no bye")
	}
	if err := <-errc; err != nil {
		t.Errorf("Run: %v", err)
	}
}

func TestProbe(t *testing.T) {
	var conns atomic.Int32
	url := serve(t, &conns, func(g *gatewayConn) {
		expectHello(t, g)
		g.send(protocol.Welcome{Type: "welcome", ConnectionID: "p", Name: "Probe", PingIntervalMs: 30000, MaxRows: 500})
		g.read() // bye
	})
	w, r, err := Probe(context.Background(), options(url))
	if err != nil || r != nil || w == nil || w.Name != "Probe" {
		t.Errorf("probe: %v %v %v", w, r, err)
	}
	if _, _, err := Probe(context.Background(), options("ws://127.0.0.1:1/v1/agent")); err == nil {
		t.Error("unreachable gateway should fail")
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		d := backoff(attempt, time.Second, 60*time.Second)
		if d < 500*time.Millisecond || d > 60*time.Second {
			t.Errorf("attempt %d: %v out of bounds", attempt, d)
		}
	}
	if d := backoff(0, time.Second, 60*time.Second); d > time.Second {
		t.Errorf("first retry should be at most 1 s, got %v", d)
	}
}
