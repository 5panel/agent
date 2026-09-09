// Package protocol is the Go side of docs/agent/protocol.md: the message
// shapes that travel over the WebSocket, the limits every party agrees on,
// and the validation that runs on each request before it can touch the
// database. Field names are snake_case on the wire, hence the JSON tags.
//
// Nothing in this package talks to a database or a socket; it is pure data
// and checks, which is what makes it easy to audit and to test.
package protocol

import "time"

// Version is the protocol major version carried in hello.
const Version = 1

// Message type names, as they appear in the "type" field.
const (
	TypeHello    = "hello"
	TypeWelcome  = "welcome"
	TypeReject   = "reject"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeBye      = "bye"
	TypeError    = "error"
	TypeQueryReq = "query_req"
	TypeQueryRes = "query_res"
)

// Engines the agent can front.
const (
	EngineMySQL   = "mysql"
	EngineMongoDB = "mongodb"
)

// Operations a request can ask for.
const (
	OpList      = "list"
	OpProfile   = "profile"
	OpFind      = "find"
	OpCount     = "count"
	OpAggregate = "aggregate"
	OpWrite     = "write"
)

// Reject reasons the gateway may answer hello with.
const (
	RejectBadToken = "bad_token"
	RejectProtocol = "protocol"
	RejectReplaced = "replaced"
)

// Error codes carried in a query_res with status "error".
const (
	ErrTimeout     = "timeout"
	ErrDB          = "db"
	ErrInvalid     = "invalid"
	ErrRefused     = "refused"
	ErrUnsupported = "unsupported"
)

// Write actions.
const (
	ActionInsert = "insert"
	ActionUpdate = "update"
	ActionDelete = "delete"
)

// Limits shared with lib/agent/protocol.ts (LIMITS there). Keep the two in
// step: a request the platform considers valid must be valid here too.
const (
	HelloTimeout          = 10 * time.Second
	DefaultPingInterval   = 30 * time.Second
	MaxRows               = 500
	MaxFields             = 64
	MaxMetrics            = 8
	MaxSample             = 1000
	DefaultSample         = 300
	DefaultExamples       = 3
	MaxExamples           = 10
	DefaultTimeoutMs      = 3_000
	MinTimeoutMs          = 100
	MaxTimeoutMs          = 15_000
	MaxFilterDepth        = 6
	MaxFilterNodes        = 64
	MaxInValues           = 100
	MaxSortKeys           = 4
	MaxSkip               = 1_000_000
	DefaultFindLimit      = 100
	DefaultAggregateLimit = 50
	DefaultWriteLimit     = 1
	MaxWriteLimit         = 100
)

// FilterOps lists every condition operator, in the order the doc gives them.
var FilterOps = []string{"eq", "ne", "gt", "gte", "lt", "lte", "in", "nin", "contains", "starts", "exists"}

// MetricFns lists every aggregate function.
var MetricFns = []string{"count", "sum", "avg", "min", "max"}

// Capabilities every build announces; "write" is appended by the executor
// when the configuration turns writes on for at least one collection.
var BaseCapabilities = []string{OpList, OpProfile, OpFind, OpCount, OpAggregate}
