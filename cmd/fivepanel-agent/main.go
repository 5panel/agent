// Command fivepanel-agent is the FivePanel database agent. It runs next to
// a MySQL/MariaDB or MongoDB database, dials out to the FivePanel gateway
// over one WebSocket and answers structured read requests locally. See
// README.md for the security model and the configuration reference.
//
// Exit codes: 0 success, 1 failure, 2 usage error or rejected token.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/5panel/agent/internal/config"
	"github.com/5panel/agent/internal/db"
	"github.com/5panel/agent/internal/db/mongo"
	"github.com/5panel/agent/internal/db/mysql"
	"github.com/5panel/agent/internal/gateway"
	"github.com/5panel/agent/internal/protocol"
	"github.com/5panel/agent/internal/service"
	"github.com/5panel/agent/internal/version"
)

const (
	exitOK       = 0
	exitFailure  = 1
	exitBadUsage = 2
	exitBadToken = 2
)

const usageText = `fivepanel-agent - connects your database to FivePanel

Usage:
  fivepanel-agent <command> [flags]

Commands:
  setup      Ask for the token and database, test both, write config.yaml
  run        Run in the foreground (what the service runs)
  test       Check the database and the gateway with the current config
  service    install | uninstall | start | stop | restart | status
  version    Print version and build information

Run "fivepanel-agent <command> --help" for the flags of a command.

Environment overrides: FIVEPANEL_TOKEN, FIVEPANEL_GATEWAY, FIVEPANEL_CONFIG,
DATABASE_URL, DATABASE_TYPE, LOG_LEVEL.
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return exitBadUsage
	}
	switch args[0] {
	case "setup":
		return cmdSetup(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "test":
		return cmdTest(args[1:])
	case "service":
		return cmdService(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("fivepanel-agent %s (commit %s, built %s) %s %s/%s\n",
			version.Version, version.Commit, version.Date, version.GoVersion(), runtime.GOOS, runtime.GOARCH)
		return exitOK
	case "help", "--help", "-h":
		fmt.Print(usageText)
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usageText)
	return exitBadUsage
}

// ---------------------------------------------------------------- helpers

func newFlagSet(name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: fivepanel-agent %s [flags]\n\n%s\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) (int, bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, false
		}
		return exitBadUsage, false
	}
	return exitOK, true
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	if h := service.Logger(lvl); h != nil {
		return slog.New(h)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// openStore builds the engine for the configuration. Connections are made
// lazily; Ping to check.
func openStore(cfg *config.Config) (db.Store, error) {
	switch cfg.Database.Type {
	case protocol.EngineMySQL:
		return mysql.Open(cfg.Database.URL, mysql.Pool{
			MaxOpen:     cfg.Database.MaxOpenConns,
			MaxIdle:     cfg.Database.MaxIdleConns,
			MaxLifetime: cfg.ConnMaxLifetime(),
		})
	case protocol.EngineMongoDB:
		return mongo.Open(cfg.Database.URL, cfg.Database.Name, mongo.Pool{MaxOpen: cfg.Database.MaxOpenConns})
	}
	return nil, fmt.Errorf("unknown database.type %q", cfg.Database.Type)
}

// describe gives host and database name for messages; never credentials.
func describe(cfg *config.Config) (host, name string) {
	if cfg.Database.Type == protocol.EngineMongoDB {
		host, name = mongo.Describe(cfg.Database.URL)
		if cfg.Database.Name != "" {
			name = cfg.Database.Name
		}
		return host, name
	}
	return mysql.Describe(cfg.Database.URL)
}

func limitsOf(cfg *config.Config) db.Limits {
	return db.Limits{
		ReadOnly:       cfg.Security.ReadOnly,
		WriteAllow:     cfg.Writes.Allow,
		QueryTimeout:   cfg.QueryTimeout(),
		MaxRows:        cfg.Security.MaxRowsLimit,
		MaxResultBytes: cfg.Security.MaxResultBytes,
		LogQueries:     cfg.Logging.LogQueries,
	}
}

func agentInfo() protocol.AgentInfo {
	host, _ := os.Hostname()
	return protocol.AgentInfo{Version: version.Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: host}
}

func gatewayOptions(cfg *config.Config, exec *db.Executor, log *slog.Logger) gateway.Options {
	return gateway.Options{
		URL:           cfg.Gateway,
		Token:         cfg.Token,
		Engine:        cfg.Database.Type,
		Database:      exec.Store.Name(),
		Agent:         agentInfo(),
		Capabilities:  exec.Capabilities(),
		MaxConcurrent: cfg.Security.MaxConcurrent,
		Handler:       exec,
		Log:           log,
	}
}

// -------------------------------------------------------------------- run

func cmdRun(args []string) int {
	fs := newFlagSet("run", "Run the agent in the foreground until interrupted.")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	err := service.Run(func(ctx context.Context) error { return runAgent(ctx, *cfgPath) })
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, gateway.ErrBadToken):
		return exitBadToken
	}
	fmt.Fprintln(os.Stderr, "error:", err)
	return exitFailure
}

func runAgent(ctx context.Context, cfgPath string) error {
	cfg, path, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Logging.Level)
	if path == "" {
		path = "(environment)"
	}
	host, name := describe(cfg)
	log.Info("fivepanel-agent starting", "version", version.Version, "config", path, "engine", cfg.Database.Type, "host", host, "database", name, "read_only", cfg.Security.ReadOnly)

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = store.Close(cctx)
	}()

	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = store.Ping(pctx)
	cancel()
	if err != nil {
		log.Warn("database is not reachable yet; requests will fail until it is", "error", err)
	} else {
		log.Info("database connected")
	}

	exec := &db.Executor{Store: store, Limits: limitsOf(cfg), Log: log}
	if exec.WritesEnabled() {
		log.Warn("writes are enabled", "collections", strings.Join(cfg.Writes.Allow, ","))
	}
	err = gateway.Run(ctx, gatewayOptions(cfg, exec, log))
	if errors.Is(err, gateway.ErrBadToken) {
		log.Error("the gateway rejected the token; it was revoked or never valid. Create a new token in FivePanel and run `fivepanel-agent setup` again.")
		return err
	}
	log.Info("fivepanel-agent stopped")
	return err
}

// ------------------------------------------------------------------- test

func cmdTest(args []string) int {
	fs := newFlagSet("test", "Connect to the database and the gateway and report; exit 1 on any failure.")
	cfgPath := fs.String("config", "", "path to config.yaml")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, path, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return exitFailure
	}
	if path == "" {
		path = "(environment)"
	}
	fmt.Println("config   ", path)

	failed := false
	store, err := openStore(cfg)
	if err != nil {
		fmt.Println("database  FAILED:", err)
		return exitFailure
	}
	defer store.Close(context.Background())

	host, name := describe(cfg)
	if err := checkDatabase(store, cfg.Database.Type, host, name); err != nil {
		fmt.Println("database  FAILED:", err)
		failed = true
	}

	exec := &db.Executor{Store: store, Limits: limitsOf(cfg)}
	code := checkGateway(gatewayOptions(cfg, exec, slog.New(slog.NewTextHandler(io.Discard, nil))))
	if code == exitBadToken {
		return code
	}
	if code != exitOK {
		failed = true
	}
	if failed {
		return exitFailure
	}
	fmt.Println("all good")
	return exitOK
}

func checkDatabase(store db.Store, engine, host, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := store.Ping(ctx); err != nil {
		return err
	}
	fmt.Printf("database  %s %s @ %s ok (%d ms)\n", engine, name, host, time.Since(start).Milliseconds())
	cols, err := store.List(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("collections %d\n", len(cols))
	for _, c := range cols {
		fmt.Printf("  %-40s %d\n", c.Name, c.Count)
	}
	return nil
}

func checkGateway(o gateway.Options) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	welcome, reject, err := gateway.Probe(ctx, o)
	switch {
	case err != nil:
		fmt.Printf("gateway   %s FAILED: %v\n", o.URL, err)
		return exitFailure
	case reject != nil:
		fmt.Printf("gateway   %s rejected: %s\n", o.URL, reject.Reason)
		if reject.Reason == protocol.RejectBadToken {
			fmt.Println("          the token is unknown or revoked; create a new one in FivePanel")
			return exitBadToken
		}
		return exitFailure
	}
	fmt.Printf("gateway   %s welcome %q (connection %s, %d ms)\n", o.URL, welcome.Name, welcome.ConnectionID, time.Since(start).Milliseconds())
	return exitOK
}

// ------------------------------------------------------------------ setup

func cmdSetup(args []string) int {
	fs := newFlagSet("setup", "Collect the token and database URL (prompting for what is missing), test both and write config.yaml.")
	token := fs.String("token", "", "FivePanel connection token (fp_live_...)")
	dbURL := fs.String("db", "", "database URL: mysql://user:pass@host:3306/db, user:pass@tcp(host:3306)/db or mongodb://...")
	engine := fs.String("engine", "", "mysql or mongodb (detected from the URL when omitted)")
	gw := fs.String("gateway", "", "gateway URL (default "+config.DefaultGateway+")")
	cfgPath := fs.String("config", "", "where to write config.yaml (default "+config.SystemPath()+")")
	yes := fs.Bool("yes", false, "no prompts: accept defaults, overwrite an existing file, save even if a check fails")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(label, current string) string {
		if current != "" || *yes {
			return current
		}
		return prompt(in, label)
	}

	cfg := config.Default()
	cfg.Token = ask("FivePanel token (fp_live_...)", firstOf(*token, os.Getenv(config.EnvToken)))
	cfg.Database.URL = ask("Database URL (mysql://user:pass@host:3306/db or mongodb://user:pass@host:27017/db)", firstOf(*dbURL, os.Getenv(config.EnvDBURL)))
	cfg.Database.Type = firstOf(*engine, os.Getenv(config.EnvDBType), config.DetectEngine(cfg.Database.URL))
	if cfg.Database.Type == "" {
		cfg.Database.Type = ask("Engine (mysql or mongodb)", "")
	}
	cfg.Gateway = firstOf(*gw, os.Getenv(config.EnvGateway), config.DefaultGateway)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitBadUsage
	}
	if !protocol.IsToken(cfg.Token) {
		fmt.Println("note: the token does not look like fp_live_ followed by 43 characters; continuing anyway")
	}

	// 1. Database.
	store, err := openStore(&cfg)
	if err == nil {
		host, name := describe(&cfg)
		err = checkDatabase(store, cfg.Database.Type, host, name)
		store.Close(context.Background())
	}
	if err != nil {
		fmt.Println("database  FAILED:", err)
		if !*yes && !confirm(in, "Save the configuration anyway?") {
			return exitFailure
		}
	}

	// 2. Gateway.
	exec := &db.Executor{Limits: limitsOf(&cfg)}
	o := gateway.Options{URL: cfg.Gateway, Token: cfg.Token, Engine: cfg.Database.Type, Database: "setup", Agent: agentInfo(), Capabilities: exec.Capabilities(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	switch checkGateway(o) {
	case exitBadToken:
		return exitBadToken
	case exitFailure:
		if !*yes && !confirm(in, "Save the configuration anyway?") {
			return exitFailure
		}
	}

	// 3. Write.
	path := firstOf(*cfgPath, os.Getenv(config.EnvConfig), config.SystemPath())
	if _, statErr := os.Stat(path); statErr == nil && !*yes && !confirm(in, path+" exists. Overwrite?") {
		return exitFailure
	}
	if err := config.Save(path, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "could not write", path+":", err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "run setup with administrator rights (sudo on Linux) or pass --config to a writable path")
		}
		return exitFailure
	}
	fmt.Println("wrote    ", path, "(mode 0600)")
	fmt.Println()
	fmt.Println("Next:")
	fmt.Printf("  fivepanel-agent service install --config %q   # run at boot\n", path)
	fmt.Println("  fivepanel-agent service start")
	fmt.Printf("  fivepanel-agent run --config %q               # or run in the foreground\n", path)
	return exitOK
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func prompt(in *bufio.Reader, label string) string {
	for {
		fmt.Printf("%s: ", label)
		line, err := in.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
		if err != nil {
			return ""
		}
	}
}

func confirm(in *bufio.Reader, question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// ---------------------------------------------------------------- service

func cmdService(args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "Usage: fivepanel-agent service install|uninstall|start|stop|restart|status [--config PATH]")
		return exitBadUsage
	}
	action := args[0]
	fs := newFlagSet("service "+action, "Manage the system service (systemd on Linux, a Windows service on Windows).")
	cfgPath := fs.String("config", "", "config.yaml the service runs with (default: the resolved config path)")
	if code, ok := parseFlags(fs, args[1:]); !ok {
		return code
	}
	path, _ := config.Resolve(*cfgPath)
	out, err := service.Control(action, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitFailure
	}
	fmt.Println(out)
	if action == "install" {
		fmt.Println("config   ", path)
		fmt.Println("start it with: fivepanel-agent service start")
	}
	return exitOK
}
