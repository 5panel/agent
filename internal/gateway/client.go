// Package gateway is the WebSocket client: it dials OUT to the FivePanel
// gateway, performs the hello/welcome handshake, keeps the heartbeat, and
// feeds requests to the executor through a bounded worker pool. It is the
// only network client in the agent; the database side never accepts a
// connection from anywhere.
//
// Connection life cycle (protocol.md sections 1, 2 and 4):
//
//	dial -> hello -> welcome | reject
//	  welcome: workers start; read loop until the socket drops
//	  reject bad_token: return ErrBadToken (the caller exits, no retry)
//	  reject replaced: return nil quietly (a newer agent took over)
//	  anything else: back off 1 s .. 60 s with jitter and dial again
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/5panel/agent/internal/protocol"
)

// Handler answers one request; the executor implements it.
type Handler interface {
	Execute(ctx context.Context, req *protocol.QueryReq, gatewayMaxRows int) []byte
}

// Options configure a client.
type Options struct {
	URL          string
	Token        string
	Engine       string
	Database     string
	Agent        protocol.AgentInfo
	Capabilities []string

	MaxConcurrent int
	Handler       Handler
	Log           *slog.Logger

	// Timing knobs; zero means the protocol defaults (10 s dial, 1 s..60 s
	// backoff). Tests lower them.
	DialTimeout time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

// ErrBadToken is returned by Run when the gateway answered reject with
// reason bad_token. The caller should exit rather than retry.
var ErrBadToken = errors.New("the gateway rejected the token (reject: bad_token); check the token in the configuration")

// errReplaced is the internal signal for reject: replaced.
var errReplaced = errors.New("replaced by a newer agent with the same token")

// errHeartbeat is the internal signal for two unanswered pings.
var errHeartbeat = errors.New("two pings went unanswered")

const (
	readLimit    = 1 << 20 // bytes per incoming frame; requests are small
	queueDepth   = 1024    // requests waiting for a worker
	writeTimeout = 10 * time.Second
)

func (o *Options) defaults() {
	if o.DialTimeout <= 0 {
		o.DialTimeout = protocol.HelloTimeout
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 60 * time.Second
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 4
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// Run keeps the agent connected until ctx is cancelled (returns nil), the
// token is rejected (ErrBadToken) or the gateway replaced this agent (nil).
func Run(ctx context.Context, o Options) error {
	o.defaults()
	attempt := 0
	for {
		welcomed, err := runOnce(ctx, &o)
		switch {
		case errors.Is(err, ErrBadToken):
			return err
		case errors.Is(err, errReplaced):
			o.Log.Info("replaced by a newer agent; exiting")
			return nil
		case ctx.Err() != nil:
			return nil
		}
		if welcomed {
			attempt = 0
		}
		delay := backoff(attempt, o.MinBackoff, o.MaxBackoff)
		attempt++
		if err != nil {
			o.Log.Warn("gateway connection lost", "error", err, "retry_in", delay.Round(time.Millisecond))
		} else {
			o.Log.Warn("gateway closed the connection", "retry_in", delay.Round(time.Millisecond))
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil
		}
	}
}

// backoff is min(max, min*2^attempt) with +-50 % jitter.
func backoff(attempt int, minD, maxD time.Duration) time.Duration {
	d := minD
	for i := 0; i < attempt && d < maxD; i++ {
		d *= 2
	}
	if d > maxD {
		d = maxD
	}
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	return time.Duration(half + rand.Int64N(half+1))
}

// Probe dials once, sends hello and reports the gateway's answer, then
// says bye. Used by `setup` and `test`.
func Probe(ctx context.Context, o Options) (*protocol.Welcome, *protocol.Reject, error) {
	o.defaults()
	conn, err := dial(ctx, &o)
	if err != nil {
		return nil, nil, err
	}
	defer conn.CloseNow()
	first, err := handshake(ctx, conn, &o)
	if err != nil {
		return nil, nil, err
	}
	switch m := first.(type) {
	case *protocol.Welcome:
		_ = write(conn, protocol.Bye{Type: protocol.TypeBye, Reason: "probe"})
		_ = conn.Close(websocket.StatusNormalClosure, "probe")
		return m, nil, nil
	case *protocol.Reject:
		return nil, m, nil
	}
	return nil, nil, fmt.Errorf("unexpected first message %T", first)
}

func dial(ctx context.Context, o *Options) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, o.DialTimeout)
	defer cancel()
	header := http.Header{}
	header.Set("User-Agent", "fivepanel-agent/"+o.Agent.Version)
	conn, _, err := websocket.Dial(dctx, o.URL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", o.URL, err)
	}
	conn.SetReadLimit(readLimit)
	return conn, nil
}

// handshake sends hello and returns the gateway's first message.
func handshake(ctx context.Context, conn *websocket.Conn, o *Options) (any, error) {
	hello := protocol.Hello{
		Type:         protocol.TypeHello,
		Protocol:     protocol.Version,
		Token:        o.Token,
		Engine:       o.Engine,
		Database:     o.Database,
		Agent:        o.Agent,
		Capabilities: o.Capabilities,
	}
	if err := write(conn, hello); err != nil {
		return nil, fmt.Errorf("send hello: %w", err)
	}
	hctx, cancel := context.WithTimeout(ctx, protocol.HelloTimeout)
	defer cancel()
	_, data, err := conn.Read(hctx)
	if err != nil {
		return nil, fmt.Errorf("waiting for welcome: %w", err)
	}
	msg, err := protocol.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("first frame: %w", err)
	}
	return msg, nil
}

func rejectError(r *protocol.Reject) error {
	switch r.Reason {
	case protocol.RejectBadToken:
		return ErrBadToken
	case protocol.RejectReplaced:
		return errReplaced
	case protocol.RejectProtocol:
		return fmt.Errorf("the gateway does not accept protocol version %d; update the agent", protocol.Version)
	}
	return fmt.Errorf("gateway rejected the connection: %s", r.Reason)
}

// write encodes and sends one frame with its own deadline, independent of
// the session context so bye can still go out during shutdown.
func write(conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeRaw(conn, data)
}

func writeRaw(conn *websocket.Conn, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// runOnce is one connection. welcomed tells Run whether to reset backoff.
func runOnce(ctx context.Context, o *Options) (welcomed bool, err error) {
	conn, err := dial(ctx, o)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()

	first, err := handshake(ctx, conn, o)
	if err != nil {
		return false, err
	}
	var welcome *protocol.Welcome
	switch m := first.(type) {
	case *protocol.Welcome:
		welcome = m
	case *protocol.Reject:
		return false, rejectError(m)
	default:
		return false, fmt.Errorf("unexpected first message %T", first)
	}
	if welcome.PingIntervalMs <= 0 {
		welcome.PingIntervalMs = int(protocol.DefaultPingInterval / time.Millisecond)
	}
	o.Log.Info("connected to gateway", "name", welcome.Name, "connection_id", welcome.ConnectionID, "max_rows", welcome.MaxRows, "ping_interval_ms", welcome.PingIntervalMs)

	s := &session{o: o, conn: conn, welcome: welcome, queue: make(chan *protocol.QueryReq, queueDepth)}
	return true, s.loop(ctx)
}

// session is one welcomed connection.
type session struct {
	o       *Options
	conn    *websocket.Conn
	welcome *protocol.Welcome
	queue   chan *protocol.QueryReq
	wg      sync.WaitGroup
	pending atomic.Int32 // own pings not yet answered
}

func (s *session) loop(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	for i := 0; i < s.o.MaxConcurrent; i++ {
		s.wg.Add(1)
		go s.worker(ctx)
	}
	s.wg.Add(1)
	go s.pinger(ctx)

	// Shutdown watcher. The read loop must not run on a cancellable
	// context (the library closes the socket when that context ends, which
	// would lose the bye), so on shutdown this goroutine says bye and closes
	// the socket, which in turn ends the read loop.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		if parent.Err() != nil {
			_ = write(s.conn, protocol.Bye{Type: protocol.TypeBye, Reason: "shutdown"})
			_ = s.conn.Close(websocket.StatusNormalClosure, "shutdown")
		}
	}()

	err := s.readLoop()
	cancel()
	s.wg.Wait()
	if parent.Err() != nil {
		return nil
	}
	return err
}

func (s *session) readLoop() error {
	for {
		_, data, err := s.conn.Read(context.Background())
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return err
		}
		if err := s.handle(data); err != nil {
			return err
		}
	}
}

// handle dispatches one frame; a returned error ends the session.
func (s *session) handle(data []byte) error {
	msg, err := protocol.Decode(data)
	if err != nil {
		var unknown *protocol.UnknownTypeError
		if errors.As(err, &unknown) {
			s.o.Log.Debug("unknown message type", "type", unknown.Type)
		} else {
			s.o.Log.Warn("bad frame from gateway", "error", err)
		}
		return write(s.conn, protocol.ErrorMsg{Type: protocol.TypeError, Message: err.Error()})
	}
	switch m := msg.(type) {
	case *protocol.Ping:
		return write(s.conn, protocol.Pong{Type: protocol.TypePong, T: m.T})
	case *protocol.Pong:
		s.pending.Store(0)
	case *protocol.Reject:
		return rejectError(m)
	case *protocol.QueryReq:
		if m.QueryID == "" {
			return write(s.conn, protocol.ErrorMsg{Type: protocol.TypeError, Message: "query_req without query_id"})
		}
		select {
		case s.queue <- m:
		default:
			res := protocol.NewError(m.QueryID, protocol.ErrRefused, "agent request queue is full")
			return write(s.conn, res)
		}
	case *protocol.ErrorMsg:
		s.o.Log.Warn("gateway reported an error", "message", m.Message)
	case *protocol.Bye:
		return errors.New("gateway said bye")
	case *protocol.Welcome:
		s.o.Log.Debug("duplicate welcome ignored")
	default:
		return write(s.conn, protocol.ErrorMsg{Type: protocol.TypeError, Message: fmt.Sprintf("unexpected message %T", msg)})
	}
	return nil
}

// worker executes queued requests in arrival order, MaxConcurrent at a
// time across all workers.
func (s *session) worker(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.queue:
			data := s.o.Handler.Execute(ctx, req, s.welcome.MaxRows)
			if ctx.Err() != nil {
				return
			}
			if err := writeRaw(s.conn, data); err != nil {
				s.o.Log.Debug("could not send result", "query_id", req.QueryID, "error", err)
			}
		}
	}
}

// pinger sends the agent's own ping every interval and drops the socket
// after two go unanswered; the read loop then returns and Run reconnects.
func (s *session) pinger(ctx context.Context) {
	defer s.wg.Done()
	interval := time.Duration(s.welcome.PingIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.pending.Load() >= 2 {
				s.o.Log.Warn("gateway stopped answering pings; reconnecting")
				_ = s.conn.Close(websocket.StatusPolicyViolation, errHeartbeat.Error())
				return
			}
			s.pending.Add(1)
			if err := write(s.conn, protocol.Ping{Type: protocol.TypePing, T: time.Now().UnixMilli()}); err != nil {
				return
			}
		}
	}
}
