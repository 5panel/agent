// The executor: validates a request, applies the agent's own limits, runs
// it against the Store and produces the query_res frame. It is the one
// place a request's fate is decided, so the security rules read top to
// bottom here:
//
//   - nothing reaches the database before Validate passes;
//   - the deadline is min(request timeout, security.query_timeout_ms);
//   - rows are capped by min(request limit, security.max_rows_limit, the
//     gateway's max_rows);
//   - a write needs read_only: false AND the collection in writes.allow, and
//     an update or delete always needs a filter;
//   - an answer larger than security.max_result_bytes is replaced with an
//     "invalid" error rather than sent.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/5panel/agent/internal/profile"
	"github.com/5panel/agent/internal/protocol"
)

// Limits are the security settings of the configuration.
type Limits struct {
	ReadOnly       bool
	WriteAllow     []string
	QueryTimeout   time.Duration
	MaxRows        int
	MaxResultBytes int
	LogQueries     bool
}

// Executor runs requests against one Store.
type Executor struct {
	Store  Store
	Limits Limits
	Log    *slog.Logger
}

// Capabilities lists the operations to announce in hello.
func (e *Executor) Capabilities() []string {
	caps := append([]string{}, protocol.BaseCapabilities...)
	if e.WritesEnabled() {
		caps = append(caps, protocol.OpWrite)
	}
	return caps
}

// WritesEnabled reports whether any write can ever be accepted.
func (e *Executor) WritesEnabled() bool {
	return !e.Limits.ReadOnly && len(e.Limits.WriteAllow) > 0
}

// Execute answers one request; the return value is the encoded query_res.
// gatewayMaxRows is the cap the welcome message announced (0 = none).
func (e *Executor) Execute(ctx context.Context, req *protocol.QueryReq, gatewayMaxRows int) []byte {
	start := time.Now()
	res := e.run(ctx, req, gatewayMaxRows)
	res.QueryID = req.QueryID
	res.ExecutionTimeMs = time.Since(start).Milliseconds()

	data, err := json.Marshal(res)
	if err != nil {
		res = protocol.NewError(req.QueryID, protocol.ErrDB, "result could not be encoded: "+err.Error())
		res.ExecutionTimeMs = time.Since(start).Milliseconds()
		data, _ = json.Marshal(res)
	}
	if e.Limits.MaxResultBytes > 0 && len(data) > e.Limits.MaxResultBytes {
		res = protocol.NewError(req.QueryID, protocol.ErrInvalid,
			fmt.Sprintf("result of %d bytes exceeds security.max_result_bytes (%d)", len(data), e.Limits.MaxResultBytes))
		res.ExecutionTimeMs = time.Since(start).Milliseconds()
		data, _ = json.Marshal(res)
	}
	e.logQuery(req, res)
	return data
}

func (e *Executor) run(ctx context.Context, req *protocol.QueryReq, gatewayMaxRows int) *protocol.QueryRes {
	if p := req.Validate(); p != "" {
		if p == "unknown operation" {
			return protocol.NewError(req.QueryID, protocol.ErrUnsupported, fmt.Sprintf("operation %q is not supported by this agent", req.Op))
		}
		return protocol.NewError(req.QueryID, protocol.ErrInvalid, p)
	}

	timeout := time.Duration(req.Timeout()) * time.Millisecond
	if e.Limits.QueryTimeout > 0 && e.Limits.QueryTimeout < timeout {
		timeout = e.Limits.QueryTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		res *protocol.QueryRes
		err error
	)
	switch req.Op {
	case protocol.OpList:
		var cols []protocol.CollectionInfo
		cols, err = e.Store.List(ctx)
		res = protocol.NewList(req.QueryID, cols)
	case protocol.OpProfile:
		res, err = e.profile(ctx, req)
	case protocol.OpFind:
		res, err = e.find(ctx, req, gatewayMaxRows)
	case protocol.OpCount:
		var n int64
		n, err = e.Store.Count(ctx, req.Collection, req.Filter)
		res = protocol.NewCount(req.QueryID, n)
	case protocol.OpAggregate:
		res, err = e.aggregate(ctx, req, gatewayMaxRows)
	case protocol.OpWrite:
		res, err = e.write(ctx, req)
	}
	if err != nil {
		return e.errorRes(ctx, req, timeout, err)
	}
	return res
}

func (e *Executor) profile(ctx context.Context, req *protocol.QueryReq) (*protocol.QueryRes, error) {
	sample := protocol.IntOr(req.Sample, protocol.DefaultSample)
	if sample > protocol.MaxSample {
		sample = protocol.MaxSample
	}
	examples := protocol.IntOr(req.Examples, protocol.DefaultExamples)
	docs, total, err := e.Store.Sample(ctx, req.Collection, sample)
	if err != nil {
		return nil, err
	}
	fields := profile.Build(docs, profile.Options{Examples: examples})
	return protocol.NewProfile(req.QueryID, &protocol.CollectionProfile{
		Collection: req.Collection,
		Sampled:    len(docs),
		Count:      total,
		Fields:     fields,
	}), nil
}

func (e *Executor) rowCap(requested, def, gatewayMaxRows int) int {
	limit := def
	if requested > 0 {
		limit = requested
	}
	if e.Limits.MaxRows > 0 && limit > e.Limits.MaxRows {
		limit = e.Limits.MaxRows
	}
	if gatewayMaxRows > 0 && limit > gatewayMaxRows {
		limit = gatewayMaxRows
	}
	if limit > protocol.MaxRows {
		limit = protocol.MaxRows
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

func (e *Executor) find(ctx context.Context, req *protocol.QueryReq, gatewayMaxRows int) (*protocol.QueryRes, error) {
	limit := e.rowCap(protocol.IntOr(req.Limit, 0), protocol.DefaultFindLimit, gatewayMaxRows)
	// One row past the cap tells whether the cap cut the result.
	rows, err := e.Store.Find(ctx, FindQuery{
		Collection: req.Collection,
		Fields:     req.Fields,
		Filter:     req.Filter,
		Sort:       req.Sort,
		Limit:      limit + 1,
		Skip:       protocol.IntOr(req.Skip, 0),
	})
	if err != nil {
		return nil, err
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	return protocol.NewRows(req.QueryID, rows, &truncated), nil
}

func (e *Executor) aggregate(ctx context.Context, req *protocol.QueryReq, gatewayMaxRows int) (*protocol.QueryRes, error) {
	limit := e.rowCap(protocol.IntOr(req.Limit, 0), protocol.DefaultAggregateLimit, gatewayMaxRows)
	metrics := make([]protocol.Metric, len(req.Metrics))
	for i, m := range req.Metrics {
		m.As = protocol.MetricKey(m)
		metrics[i] = m
	}
	rows, err := e.Store.Aggregate(ctx, AggregateQuery{
		Collection: req.Collection,
		Filter:     req.Filter,
		GroupBy:    protocol.StringOr(req.GroupBy, ""),
		Metrics:    metrics,
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}
	return protocol.NewRows(req.QueryID, rows, nil), nil
}

// write enforces the allowlist before anything else. The refusals are
// deliberate answers, not errors: the request was fine, this agent just
// does not do that.
func (e *Executor) write(ctx context.Context, req *protocol.QueryReq) (*protocol.QueryRes, error) {
	if e.Limits.ReadOnly {
		return nil, Refused("this agent is read-only (security.read_only: true)")
	}
	if !e.allowed(req.Collection) {
		return nil, Refused("collection %q is not in writes.allow", req.Collection)
	}
	if req.Action != protocol.ActionInsert && req.Filter == nil {
		return nil, Refused("%s without a filter is refused", req.Action)
	}
	limit := protocol.IntOr(req.Limit, protocol.DefaultWriteLimit)
	if limit > protocol.MaxWriteLimit {
		limit = protocol.MaxWriteLimit
	}
	n, err := e.Store.Write(ctx, WriteQuery{
		Collection: req.Collection,
		Action:     req.Action,
		Values:     req.Values,
		Set:        req.Set,
		Filter:     req.Filter,
		Limit:      limit,
	})
	if err != nil {
		return nil, err
	}
	return protocol.NewAffected(req.QueryID, n), nil
}

func (e *Executor) allowed(collection string) bool {
	for _, c := range e.Limits.WriteAllow {
		if c == collection {
			return true
		}
	}
	return false
}

// errorRes maps an engine error to a protocol error.
func (e *Executor) errorRes(ctx context.Context, req *protocol.QueryReq, timeout time.Duration, err error) *protocol.QueryRes {
	if code := CodeOf(err); code != "" {
		var de *Error
		errors.As(err, &de)
		return protocol.NewError(req.QueryID, code, de.Message)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return protocol.NewError(req.QueryID, protocol.ErrTimeout, fmt.Sprintf("query exceeded %d ms", timeout.Milliseconds()))
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return protocol.NewError(req.QueryID, protocol.ErrDB, "query cancelled")
	}
	return protocol.NewError(req.QueryID, protocol.ErrDB, strings.TrimSpace(err.Error()))
}

func (e *Executor) logQuery(req *protocol.QueryReq, res *protocol.QueryRes) {
	if e.Log == nil {
		return
	}
	if res.Status != "ok" {
		e.Log.Warn("query failed", "op", req.Op, "collection", req.Collection, "ms", res.ExecutionTimeMs, "code", res.Error.Code, "error", res.Error.Message)
		return
	}
	if !e.Limits.LogQueries {
		return
	}
	rows := -1
	switch {
	case res.Rows != nil:
		rows = len(res.Rows)
	case res.Collections != nil:
		rows = len(res.Collections)
	case res.Profile != nil:
		rows = res.Profile.Sampled
	case res.Count != nil:
		rows = int(*res.Count)
	case res.Affected != nil:
		rows = int(*res.Affected)
	}
	e.Log.Info("query", "op", req.Op, "collection", req.Collection, "ms", res.ExecutionTimeMs, "rows", rows)
}
