// Package config loads config.yaml, applies environment overrides and
// fills in defaults. The keys match the public documentation (token,
// database.*, security.*, logging.*) plus gateway, writes.allow,
// security.max_concurrent and security.max_result_bytes.
//
// The token and the database URL are secrets: the file is written with
// mode 0600 and nothing in this package ever prints them.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/5panel/agent/internal/protocol"
)

// DefaultGateway is the production endpoint.
const DefaultGateway = "wss://gateway.fivepanel.io/v1/agent"

// Environment variable names.
const (
	EnvToken    = "FIVEPANEL_TOKEN"
	EnvGateway  = "FIVEPANEL_GATEWAY"
	EnvConfig   = "FIVEPANEL_CONFIG"
	EnvDBURL    = "DATABASE_URL"
	EnvDBType   = "DATABASE_TYPE"
	EnvLogLevel = "LOG_LEVEL"
)

// Config is the whole file.
type Config struct {
	Token    string   `yaml:"token"`
	Gateway  string   `yaml:"gateway"`
	Database Database `yaml:"database"`
	Security Security `yaml:"security"`
	Writes   Writes   `yaml:"writes"`
	Logging  Logging  `yaml:"logging"`
}

// Database is the connection block.
type Database struct {
	Type               string `yaml:"type"`
	URL                string `yaml:"url"`
	Name               string `yaml:"name,omitempty"`
	MaxOpenConns       int    `yaml:"max_open_conns"`
	MaxIdleConns       int    `yaml:"max_idle_conns"`
	ConnMaxLifetimeSec int    `yaml:"conn_max_lifetime_sec"`
}

// Security holds the limits the agent enforces on its own (protocol.md §5).
type Security struct {
	ReadOnly       bool `yaml:"read_only"`
	QueryTimeoutMs int  `yaml:"query_timeout_ms"`
	MaxRowsLimit   int  `yaml:"max_rows_limit"`
	MaxConcurrent  int  `yaml:"max_concurrent"`
	MaxResultBytes int  `yaml:"max_result_bytes"`
}

// Writes is the allowlist consulted only when read_only is false.
type Writes struct {
	Allow []string `yaml:"allow"`
}

// Logging controls verbosity.
type Logging struct {
	Level      string `yaml:"level"`
	LogQueries bool   `yaml:"log_queries"`
}

// Default returns the documented defaults.
func Default() Config {
	return Config{
		Gateway: DefaultGateway,
		Database: Database{
			MaxOpenConns:       10,
			MaxIdleConns:       5,
			ConnMaxLifetimeSec: 300,
		},
		Security: Security{
			ReadOnly:       true,
			QueryTimeoutMs: 3000,
			MaxRowsLimit:   500,
			MaxConcurrent:  4,
			MaxResultBytes: 4_000_000,
		},
		Writes:  Writes{Allow: []string{}},
		Logging: Logging{Level: "info", LogQueries: false},
	}
}

// SystemPath is the per-OS default location of config.yaml.
func SystemPath() string {
	if runtime.GOOS == "windows" {
		base := os.Getenv("ProgramData")
		if base == "" {
			base = `C:\ProgramData`
		}
		return filepath.Join(base, "FivePanel", "config.yaml")
	}
	return "/etc/fivepanel/config.yaml"
}

// Resolve picks the config path: the flag, FIVEPANEL_CONFIG, ./config.yaml
// if present, else the system path. found says whether a file exists there.
func Resolve(flagPath string) (path string, found bool) {
	if flagPath != "" {
		return flagPath, exists(flagPath)
	}
	if p := os.Getenv(EnvConfig); p != "" {
		return p, exists(p)
	}
	if exists("config.yaml") {
		abs, err := filepath.Abs("config.yaml")
		if err == nil {
			return abs, true
		}
		return "config.yaml", true
	}
	p := SystemPath()
	return p, exists(p)
}

func exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Load reads the file at flagPath (or the resolved default), applies
// environment overrides and validates. A missing file is fine when the
// environment supplies token and database URL (the Docker case).
func Load(flagPath string) (*Config, string, error) {
	path, found := Resolve(flagPath)
	cfg := Default()
	if found {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, path, err
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, path, fmt.Errorf("%s: %w", path, err)
		}
	} else if flagPath != "" {
		return nil, path, fmt.Errorf("config file %s not found", path)
	}
	cfg.ApplyEnv()
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		if !found {
			return nil, path, fmt.Errorf("no config file at %s and %v; run `fivepanel-agent setup`", path, err)
		}
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if !found {
		path = ""
	}
	return &cfg, path, nil
}

// ApplyEnv overrides fields from the environment.
func (c *Config) ApplyEnv() {
	if v := os.Getenv(EnvToken); v != "" {
		c.Token = v
	}
	if v := os.Getenv(EnvGateway); v != "" {
		c.Gateway = v
	}
	if v := os.Getenv(EnvDBURL); v != "" {
		c.Database.URL = v
	}
	if v := os.Getenv(EnvDBType); v != "" {
		c.Database.Type = v
	}
	if v := os.Getenv(EnvLogLevel); v != "" {
		c.Logging.Level = v
	}
}

// Normalize trims, lower-cases enumerations and infers the engine.
func (c *Config) Normalize() {
	c.Token = strings.TrimSpace(c.Token)
	c.Gateway = strings.TrimSpace(c.Gateway)
	c.Database.URL = strings.TrimSpace(c.Database.URL)
	c.Database.Type = normalizeEngine(c.Database.Type)
	if c.Database.Type == "" {
		c.Database.Type = DetectEngine(c.Database.URL)
	}
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	if c.Gateway == "" {
		c.Gateway = DefaultGateway
	}
	if c.Writes.Allow == nil {
		c.Writes.Allow = []string{}
	}
}

func normalizeEngine(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "mysql", "mariadb":
		return protocol.EngineMySQL
	case "mongodb", "mongo":
		return protocol.EngineMongoDB
	}
	return ""
}

// DetectEngine infers the engine from the URL scheme or DSN shape.
func DetectEngine(url string) string {
	u := strings.ToLower(strings.TrimSpace(url))
	switch {
	case strings.HasPrefix(u, "mongodb://"), strings.HasPrefix(u, "mongodb+srv://"):
		return protocol.EngineMongoDB
	case strings.HasPrefix(u, "mysql://"), strings.HasPrefix(u, "mariadb://"):
		return protocol.EngineMySQL
	case strings.Contains(u, "@tcp("), strings.Contains(u, "@unix("):
		return protocol.EngineMySQL
	}
	return ""
}

// Validate checks what run needs.
func (c *Config) Validate() error {
	if c.Token == "" {
		return errors.New("token is missing")
	}
	if c.Database.URL == "" {
		return errors.New("database.url is missing")
	}
	if c.Database.Type == "" {
		return errors.New("database.type could not be inferred from the url; set it to mysql or mongodb")
	}
	if !strings.HasPrefix(c.Gateway, "wss://") && !strings.HasPrefix(c.Gateway, "ws://") {
		return errors.New("gateway must be a ws:// or wss:// url")
	}
	switch c.Logging.Level {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level %q is not debug, info, warn or error", c.Logging.Level)
	}
	if c.Security.QueryTimeoutMs <= 0 {
		return errors.New("security.query_timeout_ms must be positive")
	}
	if c.Security.MaxRowsLimit <= 0 {
		return errors.New("security.max_rows_limit must be positive")
	}
	if c.Security.MaxConcurrent <= 0 {
		return errors.New("security.max_concurrent must be positive")
	}
	for _, name := range c.Writes.Allow {
		if !protocol.IsCollectionName(name) {
			return fmt.Errorf("writes.allow entry %q is not a collection name", name)
		}
	}
	return nil
}

// QueryTimeout is security.query_timeout_ms as a duration.
func (c *Config) QueryTimeout() time.Duration {
	return time.Duration(c.Security.QueryTimeoutMs) * time.Millisecond
}

// ConnMaxLifetime is database.conn_max_lifetime_sec as a duration.
func (c *Config) ConnMaxLifetime() time.Duration {
	return time.Duration(c.Database.ConnMaxLifetimeSec) * time.Second
}

// Save writes the file with owner-only permissions, creating the directory.
func Save(path string, c *Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# FivePanel agent configuration. Keep this file private: it holds the\n# connection token and the database credentials.\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(header+string(data)), 0o600)
}

// Itoa is a tiny helper for messages.
func Itoa(n int) string { return strconv.Itoa(n) }
