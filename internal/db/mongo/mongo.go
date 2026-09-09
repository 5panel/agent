// Package mongo is the MongoDB engine on top of mongo-driver v2. Every
// query document is built with bson.D literals in filter.go; no request
// text is ever parsed as a query.
package mongo

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/protocol"
)

// Pool holds the pool settings from the configuration; only MaxOpen maps
// onto the driver (maxPoolSize).
type Pool struct {
	MaxOpen int
}

// Store is the MongoDB implementation of db.Store.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
	name   string
}

// Open connects lazily; call Ping to check reachability. The database is
// the one named in the URI unless nameOverride is set.
func Open(uri, nameOverride string, pool Pool) (*Store, error) {
	name := nameOverride
	if name == "" {
		name = DatabaseFromURI(uri)
	}
	if name == "" {
		return nil, errors.New("the MongoDB url names no database (add /<database> or set database.name)")
	}
	opts := options.Client().ApplyURI(uri).
		SetAppName("fivepanel-agent").
		SetConnectTimeout(10 * time.Second).
		SetServerSelectionTimeout(10 * time.Second)
	if pool.MaxOpen > 0 {
		opts.SetMaxPoolSize(uint64(pool.MaxOpen))
	}
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, db: client.Database(name), name: name}, nil
}

// DatabaseFromURI reads the database segment of a MongoDB URI without
// touching the credentials. It handles the multi-host form the net/url
// package cannot parse.
func DatabaseFromURI(uri string) string {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	i := strings.Index(rest, "/")
	if i < 0 {
		return ""
	}
	name := rest[i+1:]
	if q := strings.Index(name, "?"); q >= 0 {
		name = name[:q]
	}
	if u, err := url.PathUnescape(name); err == nil {
		name = u
	}
	return name
}

// Describe returns host(s) and database for logs, never the credentials.
func Describe(uri string) (host, database string) {
	rest := uri
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	return rest, DatabaseFromURI(uri)
}

// Engine returns "mongodb".
func (s *Store) Engine() string { return protocol.EngineMongoDB }

// Name returns the database name.
func (s *Store) Name() string { return s.name }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx, nil) }

// Close disconnects.
func (s *Store) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// List names collections and views (system.* excluded) with an estimate.
func (s *Store) List(ctx context.Context) ([]protocol.CollectionInfo, error) {
	specs, err := s.db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	out := []protocol.CollectionInfo{}
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "system.") {
			continue
		}
		coll := s.db.Collection(spec.Name)
		var n int64
		if spec.Type == "view" {
			n, err = coll.CountDocuments(ctx, bson.D{})
		} else {
			n, err = coll.EstimatedDocumentCount(ctx)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			n = 0
		}
		out = append(out, protocol.CollectionInfo{Name: spec.Name, Count: n})
	}
	return out, nil
}

// Sample uses $sample.
func (s *Store) Sample(ctx context.Context, collection string, n int) ([]map[string]any, int64, error) {
	coll := s.db.Collection(collection)
	total, err := coll.EstimatedDocumentCount(ctx)
	if err != nil {
		return nil, 0, err
	}
	cur, err := coll.Aggregate(ctx, mongo.Pipeline{{{Key: "$sample", Value: bson.D{{Key: "size", Value: n}}}}})
	if err != nil {
		return nil, 0, err
	}
	var raw []bson.D
	if err := cur.All(ctx, &raw); err != nil {
		return nil, 0, err
	}
	docs := make([]map[string]any, 0, len(raw))
	for _, d := range raw {
		docs = append(docs, normalizeDoc(d))
	}
	return docs, total, nil
}

// Find runs a projected, sorted, paged query.
func (s *Store) Find(ctx context.Context, q db.FindQuery) ([]protocol.Row, error) {
	filter, err := Compile(q.Filter)
	if err != nil {
		return nil, err
	}
	opts := options.Find().SetProjection(projection(q.Fields)).SetLimit(int64(q.Limit)).SetSkip(int64(q.Skip))
	if len(q.Sort) > 0 {
		opts.SetSort(sortDoc(q.Sort))
	}
	cur, err := s.db.Collection(q.Collection).Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	var raw []bson.D
	if err := cur.All(ctx, &raw); err != nil {
		return nil, err
	}
	out := make([]protocol.Row, 0, len(raw))
	for _, d := range raw {
		out = append(out, db.ProjectRow(normalizeDoc(d), q.Fields))
	}
	return out, nil
}

// Count is exact with a filter and estimated without one.
func (s *Store) Count(ctx context.Context, collection string, filter *protocol.Filter) (int64, error) {
	coll := s.db.Collection(collection)
	if filter == nil {
		return coll.EstimatedDocumentCount(ctx)
	}
	f, err := Compile(filter)
	if err != nil {
		return 0, err
	}
	return coll.CountDocuments(ctx, f)
}

// Aggregate runs $match, $group, $sort, $limit.
func (s *Store) Aggregate(ctx context.Context, q db.AggregateQuery) ([]protocol.Row, error) {
	pipeline := mongo.Pipeline{}
	if q.Filter != nil {
		f, err := Compile(q.Filter)
		if err != nil {
			return nil, err
		}
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: f}})
	}
	var groupID any
	if q.GroupBy != "" {
		groupID = "$" + protocol.StripArrays(q.GroupBy)
	}
	group := bson.D{{Key: "_id", Value: groupID}}
	countKey := ""
	for _, m := range q.Metrics {
		if m.Fn == "count" {
			group = append(group, bson.E{Key: m.As, Value: bson.D{{Key: "$sum", Value: 1}}})
			if countKey == "" {
				countKey = m.As
			}
			continue
		}
		group = append(group, bson.E{Key: m.As, Value: bson.D{{Key: "$" + m.Fn, Value: "$" + protocol.StripArrays(m.Field)}}})
	}
	pipeline = append(pipeline, bson.D{{Key: "$group", Value: group}})
	if countKey != "" {
		pipeline = append(pipeline, bson.D{{Key: "$sort", Value: bson.D{{Key: countKey, Value: -1}}}})
	} else {
		pipeline = append(pipeline, bson.D{{Key: "$sort", Value: bson.D{{Key: "_id", Value: 1}}}})
	}
	pipeline = append(pipeline, bson.D{{Key: "$limit", Value: q.Limit}})

	cur, err := s.db.Collection(q.Collection).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var raw []bson.D
	if err := cur.All(ctx, &raw); err != nil {
		return nil, err
	}
	out := make([]protocol.Row, 0, len(raw))
	for _, d := range raw {
		m := normalizeDoc(d)
		row := protocol.Row{"group": db.ToScalar(m["_id"])}
		for _, metric := range q.Metrics {
			row[metric.As] = db.ToScalar(m[metric.As])
		}
		out = append(out, row)
	}
	return out, nil
}

// Write applies one write. Update and delete first select up to Limit ids
// with the filter and then touch exactly those documents, which is how a
// row cap is enforced on operations MongoDB itself does not limit.
func (s *Store) Write(ctx context.Context, q db.WriteQuery) (int64, error) {
	coll := s.db.Collection(q.Collection)
	switch q.Action {
	case protocol.ActionInsert:
		keys := sortedKeys(q.Values)
		if _, err := coll.InsertOne(ctx, scalarDoc(q.Values, keys)); err != nil {
			return 0, err
		}
		return 1, nil
	case protocol.ActionUpdate, protocol.ActionDelete:
		if q.Filter == nil {
			return 0, db.Refused("%s without a filter is refused", q.Action)
		}
		ids, err := s.matchIDs(ctx, coll, q.Filter, q.Limit)
		if err != nil || len(ids) == 0 {
			return 0, err
		}
		target := bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}}
		if q.Action == protocol.ActionDelete {
			res, err := coll.DeleteMany(ctx, target)
			if err != nil {
				return 0, err
			}
			return res.DeletedCount, nil
		}
		keys := sortedKeys(q.Set)
		set := make(bson.D, 0, len(keys))
		for _, k := range keys {
			set = append(set, bson.E{Key: protocol.StripArrays(k), Value: q.Set[k]})
		}
		res, err := coll.UpdateMany(ctx, target, bson.D{{Key: "$set", Value: set}})
		if err != nil {
			return 0, err
		}
		return res.MatchedCount, nil
	}
	return 0, db.Invalid("bad action %q", q.Action)
}

func (s *Store) matchIDs(ctx context.Context, coll *mongo.Collection, filter *protocol.Filter, limit int) (bson.A, error) {
	f, err := Compile(filter)
	if err != nil {
		return nil, err
	}
	cur, err := coll.Find(ctx, f, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var docs []struct {
		ID any `bson:"_id"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	ids := make(bson.A, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
