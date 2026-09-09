// Package db defines what an engine must provide (Store) and runs requests
// against it (Executor). The executor is where the agent's own limits are
// enforced, engine-independently: timeouts, row caps, result size, the
// write allowlist. An engine only translates a validated request into its
// query language and hands back plain Go values.
package db

import (
	"context"

	"github.com/5panel/agent/internal/protocol"
)

// FindQuery is a validated find request with defaults and caps applied.
type FindQuery struct {
	Collection string
	Fields     []string
	Filter     *protocol.Filter
	Sort       []protocol.SortSpec
	Limit      int
	Skip       int
}

// AggregateQuery is a validated aggregate request. GroupBy is "" for the
// single-row form. Every Metric has its output key computed by
// protocol.MetricKey.
type AggregateQuery struct {
	Collection string
	Filter     *protocol.Filter
	GroupBy    string
	Metrics    []protocol.Metric
	Limit      int
}

// WriteQuery is a validated, allowed write. Limit is the row cap for update
// and delete.
type WriteQuery struct {
	Collection string
	Action     string
	Values     map[string]any
	Set        map[string]any
	Filter     *protocol.Filter
	Limit      int
}

// Store is one database engine. Every method honours ctx (the executor's
// deadline) and returns *Error for conditions the protocol names, or a
// plain error for what the database said.
type Store interface {
	// Engine is "mysql" or "mongodb".
	Engine() string
	// Name is the database name the agent is configured for.
	Name() string
	// Ping checks the connection.
	Ping(ctx context.Context) error
	// List names the collections/tables with an estimated count.
	List(ctx context.Context) ([]protocol.CollectionInfo, error)
	// Sample returns up to n documents of the collection as plain Go
	// values plus the (estimated) total count.
	Sample(ctx context.Context, collection string, n int) (docs []map[string]any, total int64, err error)
	// Find returns rows keyed by the requested paths.
	Find(ctx context.Context, q FindQuery) ([]protocol.Row, error)
	// Count counts documents matching the filter (nil = all).
	Count(ctx context.Context, collection string, filter *protocol.Filter) (int64, error)
	// Aggregate groups and computes metrics; rows carry "group" and the
	// metric keys.
	Aggregate(ctx context.Context, q AggregateQuery) ([]protocol.Row, error)
	// Write applies an insert/update/delete and returns the affected count.
	Write(ctx context.Context, q WriteQuery) (int64, error)
	// Close releases the connection pool.
	Close(ctx context.Context) error
}
