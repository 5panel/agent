# FivePanel agent

A small program that runs on your server, next to your MySQL/MariaDB or
MongoDB database, and connects it to [FivePanel](https://fivepanel.io). It
opens one **outbound** WebSocket to the FivePanel gateway and answers
structured read requests locally. Nothing listens on your server, your
database credentials never leave it, and writes are refused unless you turn
them on for specific tables.

The code is short on purpose so you can read it before running it on a
production box. Start with `internal/db/executor.go` (what every request
goes through) and `internal/db/mysql/builder.go` (how SQL is built).

## How it works

```
 your server                                     FivePanel
 ┌───────────────────────────────────────┐       ┌───────────────────┐
 │  MySQL / MariaDB / MongoDB            │       │                   │
 │        ▲                              │       │                   │
 │        │ local connection,            │       │                   │
 │        │ read-only user               │       │                   │
 │  ┌─────┴──────────┐   wss:// (out)    │       │  ┌─────────────┐  │
 │  │ fivepanel-agent├───────────────────┼──────►│  │   gateway   │  │
 │  └────────────────┘   one socket,     │       │  └──────┬──────┘  │
 │                       kept open       │       │         │         │
 └───────────────────────────────────────┘       │   your dashboards │
                                                 └───────────────────┘
```

1. The agent reads `config.yaml`, connects to the database and dials the
   gateway with your token.
2. The gateway sends requests down the socket: list collections, profile a
   collection's fields, find rows, count, aggregate.
3. The agent validates each request, runs it locally with a timeout and a
   row cap, and answers with plain JSON values. Values are only ever the
   ones a request asked for.
4. If the socket drops, the agent reconnects with backoff. If the token is
   revoked, it exits with a clear message.

The wire protocol is documented in FivePanel's `docs/agent/protocol.md`;
`internal/protocol` implements it field for field.

## Install

You need a database user for the agent. Read-only is enough:

```sql
-- MySQL / MariaDB
CREATE USER 'fivepanel_ro'@'127.0.0.1' IDENTIFIED BY 'a-long-password';
GRANT SELECT ON qbcore.* TO 'fivepanel_ro'@'127.0.0.1';
```

```js
// MongoDB
use admin
db.createUser({ user: "fivepanel_ro", pwd: "a-long-password", roles: [{ role: "read", db: "qbcore" }] })
```

### Linux (systemd)

```sh
curl -fsSL https://raw.githubusercontent.com/5panel/agent/main/install.sh | sudo sh
sudo fivepanel-agent setup          # asks for the token and database URL
```

Or in one go:

```sh
curl -fsSL https://raw.githubusercontent.com/5panel/agent/main/install.sh | \
  sudo sh -s -- --token fp_live_... --db "mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore"
```

The installer puts the binary in `/usr/local/bin`, the config in
`/etc/fivepanel/config.yaml` (mode 0600) and installs a `fivepanel-agent`
systemd unit with `Restart=always`. Logs: `journalctl -u fivepanel-agent -f`.

### Windows (service)

In an elevated PowerShell:

```powershell
irm https://raw.githubusercontent.com/5panel/agent/main/install.ps1 | iex
fivepanel-agent setup
```

Binary: `%ProgramFiles%\FivePanel\fivepanel-agent.exe` (added to PATH).
Config: `%ProgramData%\FivePanel\config.yaml`. The service is named
`fivepanel-agent` ("FivePanel Agent") and logs to the Application event log.

### Docker

```yaml
services:
  fivepanel-agent:
    image: ghcr.io/5panel/agent:latest
    restart: unless-stopped
    network_mode: host            # so 127.0.0.1:3306 is your database
    environment:
      FIVEPANEL_TOKEN: fp_live_your_token
      DATABASE_URL: mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore
      LOG_LEVEL: info
```

No config file is needed when the environment carries the token and the
URL. To use a file instead, mount it and run
`run --config /etc/fivepanel/config.yaml`.

### Manual

Download the archive for your platform from the
[releases page](https://github.com/5panel/agent/releases), verify it
against `checksums.txt`, and put `fivepanel-agent` somewhere on your PATH.

## Commands

| command | what it does |
|---|---|
| `fivepanel-agent setup [--token T] [--db URL] [--engine mysql\|mongodb] [--gateway URL] [--config PATH] [--yes]` | Prompts for anything missing, tests the database and the gateway, writes `config.yaml` with mode 0600. |
| `fivepanel-agent run [--config PATH]` | Runs in the foreground. This is what the service executes. |
| `fivepanel-agent test [--config PATH]` | Connects to the database (lists collections with counts) and to the gateway (reports welcome or reject). Exits non-zero on any failure. Prints nothing secret. |
| `fivepanel-agent service install\|uninstall\|start\|stop\|restart\|status [--config PATH]` | Manages the systemd unit / Windows service. |
| `fivepanel-agent version` | Version, commit, Go version, OS/arch. |

Exit codes: `0` ok, `1` failure, `2` usage error or rejected token.

## Configuration

The file is looked up in this order: `--config`, `$FIVEPANEL_CONFIG`,
`./config.yaml`, then `/etc/fivepanel/config.yaml` (Linux) or
`%ProgramData%\FivePanel\config.yaml` (Windows). Environment variables
override the file: `FIVEPANEL_TOKEN`, `FIVEPANEL_GATEWAY`, `DATABASE_URL`,
`DATABASE_TYPE`, `LOG_LEVEL`.

```yaml
token: "fp_live_..."                                # from the FivePanel dashboard
gateway: "wss://gateway.fivepanel.io/v1/agent"      # only change for staging

database:
  type: "mysql"                                     # mysql | mongodb (inferred from url)
  url: "mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore"
  # name: "qbcore"                                  # mongodb: when the url has no database
  max_open_conns: 10
  max_idle_conns: 5
  conn_max_lifetime_sec: 300

security:
  read_only: true                                   # refuse every write
  query_timeout_ms: 3000                            # cap on a request's timeout
  max_rows_limit: 500                               # cap on rows per answer
  max_concurrent: 4                                 # requests running at once
  max_result_bytes: 4000000                         # larger answers are not sent

writes:
  allow: []                                         # tables a write may touch (read_only: false only)

logging:
  level: "info"                                     # debug | info | warn | error
  log_queries: false                                # op, collection, ms, rows per request
```

Accepted database URLs:

- MySQL/MariaDB: `mysql://user:pass@host:3306/db?charset=utf8mb4` or the Go
  DSN `user:pass@tcp(host:3306)/db`. The agent always adds `parseTime=true`.
- MongoDB: `mongodb://user:pass@host:27017/db?authSource=admin` or
  `mongodb+srv://...`.

See `config.example.yaml` for the annotated version.

## Security model

What the agent can do is decided on your server, by your config, and the
code that enforces it is in this repository:

- **Outbound only.** The agent dials `wss://gateway.fivepanel.io`. It never
  opens a port. Your firewall needs to allow outbound 443 and nothing else.
- **Credentials stay local.** The database URL is read from the config file
  or the environment and used for the local connection only. The only
  secret that leaves the machine is the FivePanel token, sent once in the
  `hello` frame over TLS. Logs show the database host and name, never the
  URL or the token.
- **Structured requests, no raw queries.** The gateway cannot send SQL or a
  Mongo query. It sends `{op, collection, fields, filter, sort, limit}`. On
  MySQL the SQL is assembled from backtick-quoted identifiers that matched
  `^[A-Za-z_][A-Za-z0-9_]{0,63}$`, JSON paths built from validated
  segments, and `?` placeholders; every value is a query parameter. On
  MongoDB the filter tree becomes `bson.D` operators emitted by the agent;
  a request value that is an object is rejected before it gets there, so a
  `$where` or `$ne` can never be smuggled in.
- **Validation before the database.** Every request is checked against the
  protocol (identifiers, paths, filter depth and size, field counts,
  limits) and answered `invalid` when it fails. The database is not touched.
- **Your limits win.** `query_timeout_ms`, `max_rows_limit`,
  `max_concurrent` and `max_result_bytes` are applied by the agent whatever
  a request asks for; the lower of the request's and the config's value is
  used.
- **Read-only by default.** `security.read_only: true` refuses every write.
  Turning it off is not enough: a write also needs the collection in
  `writes.allow`, an update or delete needs a filter, and at most 100 rows
  are touched per request. The `write` capability is only announced to the
  gateway when both conditions hold.
- **Profiles are small.** Profiling a collection samples up to 1000 rows
  locally and sends type counts plus at most a few example values per field
  (each cut to 40 characters; long text, binary and containers give none).
- **Token revocation is immediate.** Revoking the token in FivePanel closes
  the socket with `reject: bad_token`; the agent exits (code 2) instead of
  reconnecting.

Recommended: give the agent a read-only database user even if you never
touch `read_only`. Defence in depth costs one `GRANT SELECT`.

## Building from source

Requires Go 1.25 or newer.

```sh
git clone https://github.com/5panel/agent
cd agent
go build -trimpath -ldflags "-s -w -X github.com/5panel/agent/internal/version.Version=dev" ./cmd/fivepanel-agent
go test ./...
```

Cross-compile with `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ...`.
Releases are produced by GoReleaser (`.goreleaser.yaml`) from tags `v*`.

Layout:

```
cmd/fivepanel-agent   CLI (setup, run, test, service, version)
internal/config       config.yaml, env overrides, default paths
internal/protocol     wire messages, filter grammar, validation
internal/gateway      WebSocket client: handshake, heartbeat, backoff, worker pool
internal/db           Store interface, executor (limits, allowlist, result size)
internal/db/mysql     MySQL/MariaDB engine and SQL builder
internal/db/mongo     MongoDB engine and filter compiler
internal/profile      field profiling shared by both engines
internal/service      systemd / Windows service wrapper
internal/version      build identity
```

## Protocol

The agent implements version 1 of the FivePanel agent protocol. The
normative description lives in the FivePanel platform repository as
`docs/agent/protocol.md`; the messages, limits and error codes in
`internal/protocol` mirror it one to one.

## License

MIT, see `LICENSE`.
