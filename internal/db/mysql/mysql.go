// Package mysql is the MySQL/MariaDB engine. It opens one pool with
// github.com/go-sql-driver/mysql, runs the statements builder.go renders,
// and converts what the driver returns into plain Go values for the
// executor and the profiler.
package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

// Pool holds the connection pool settings from the configuration.
type Pool struct {
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
}

// Store is the MySQL implementation of db.Store.
type Store struct {
	db   *sql.DB
	name string
}

// Open prepares the pool; the first connection is made lazily, so call
// Ping to check reachability.
func Open(rawURL string, pool Pool) (*Store, error) {
	cfg, err := ParseURL(rawURL)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	sqlDB := sql.OpenDB(connector)
	if pool.MaxOpen > 0 {
		sqlDB.SetMaxOpenConns(pool.MaxOpen)
	}
	if pool.MaxIdle > 0 {
		sqlDB.SetMaxIdleConns(pool.MaxIdle)
	}
	if pool.MaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(pool.MaxLifetime)
	}
	return &Store{db: sqlDB, name: cfg.DBName}, nil
}

// Engine returns "mysql".
func (s *Store) Engine() string { return protocol.EngineMySQL }

// Name returns the configured schema.
func (s *Store) Name() string { return s.name }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close releases the pool.
func (s *Store) Close(context.Context) error { return s.db.Close() }

// List reads tables and views of the schema with the TABLE_ROWS estimate.
func (s *Store) List(ctx context.Context) ([]protocol.CollectionInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT TABLE_NAME, COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME", s.name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.CollectionInfo{}
	for rows.Next() {
		var c protocol.CollectionInfo
		if err := rows.Scan(&c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// estimate is the TABLE_ROWS value for one table (0 when unknown).
func (s *Store) estimate(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(TABLE_ROWS, 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", s.name, table).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, db.Invalid("table %q does not exist", table)
	}
	return n, err
}

// Sample reads n rows. Small tables are sampled with ORDER BY RAND() and
// counted exactly; large ones give their first rows and the estimate.
func (s *Store) Sample(ctx context.Context, collection string, n int) ([]map[string]any, int64, error) {
	table, err := tableName(collection)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.estimate(ctx, collection)
	if err != nil {
		return nil, 0, err
	}
	stmt := "SELECT * FROM " + table + " LIMIT ?"
	if total < SampleRandomBelow {
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&total); err != nil {
			return nil, 0, err
		}
		stmt = "SELECT * FROM " + table + " ORDER BY RAND() LIMIT ?"
	}
	rows, err := s.db.QueryContext(ctx, stmt, n)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	docs, err := scanAll(rows)
	return docs, total, err
}

// Find selects the base columns and projects the requested paths in Go.
func (s *Store) Find(ctx context.Context, q db.FindQuery) ([]protocol.Row, error) {
	cols, err := BaseColumns(q.Fields)
	if err != nil {
		return nil, err
	}
	query, err := BuildFind(q.Collection, cols, q.Filter, q.Sort, q.Limit, q.Skip)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query.SQL, query.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs, err := scanAll(rows)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Row, 0, len(docs))
	for _, d := range docs {
		out = append(out, db.ProjectRow(d, q.Fields))
	}
	return out, nil
}

// Count runs SELECT COUNT(*).
func (s *Store) Count(ctx context.Context, collection string, filter *protocol.Filter) (int64, error) {
	query, err := BuildCount(collection, filter)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.QueryRowContext(ctx, query.SQL, query.Args...).Scan(&n)
	return n, err
}

// Aggregate runs the GROUP BY statement.
func (s *Store) Aggregate(ctx context.Context, q db.AggregateQuery) ([]protocol.Row, error) {
	query, err := BuildAggregate(q.Collection, q.Filter, q.GroupBy, q.Metrics, q.Limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query.SQL, query.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs, err := scanAll(rows)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Row, 0, len(docs))
	for _, d := range docs {
		row := protocol.Row{"group": db.ToScalar(d["group"])}
		for _, m := range q.Metrics {
			row[m.As] = db.ToScalar(d[m.As])
		}
		out = append(out, row)
	}
	return out, nil
}

// Write runs one of the write statements.
func (s *Store) Write(ctx context.Context, q db.WriteQuery) (int64, error) {
	var query Query
	var err error
	switch q.Action {
	case protocol.ActionInsert:
		query, err = BuildInsert(q.Collection, q.Values)
	case protocol.ActionUpdate:
		query, err = BuildUpdate(q.Collection, q.Set, q.Filter, q.Limit)
	case protocol.ActionDelete:
		query, err = BuildDelete(q.Collection, q.Filter, q.Limit)
	default:
		return 0, db.Invalid("bad action %q", q.Action)
	}
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, query.SQL, query.Args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// scanAll reads every row into a map of column -> Go value.
func scanAll(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, t := range types {
		names[i] = t.DatabaseTypeName()
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		doc := make(map[string]any, len(cols))
		for i, c := range cols {
			doc[c] = convert(vals[i], names[i])
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// convert maps a driver value to the agent's value model. The driver hands
// back []byte for text, decimals and (in the text protocol) numbers, so
// the column type decides: binary types stay bytes, decimals and numbers
// become numbers, BIT(1) becomes a boolean, everything else is text.
func convert(v any, typeName string) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	switch typeName {
	case "BINARY", "VARBINARY", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "GEOMETRY":
		return bytes.Clone(b)
	case "DECIMAL", "NEWDECIMAL", "FLOAT", "DOUBLE":
		if f, err := strconv.ParseFloat(string(b), 64); err == nil {
			return f
		}
		return string(b)
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT", "YEAR":
		if i, err := strconv.ParseInt(string(b), 10, 64); err == nil {
			return i
		}
		if u, err := strconv.ParseUint(string(b), 10, 64); err == nil {
			return u
		}
		return string(b)
	case "BIT":
		if len(b) == 1 {
			return b[0] != 0
		}
		return bytes.Clone(b)
	}
	return string(b)
}
