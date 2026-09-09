package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `token: "fp_live_file"
database:
  type: mysql
  url: "u:p@tcp(127.0.0.1:3306)/qb"
security:
  read_only: false
  max_rows_limit: 200
writes:
  allow: [bans]
logging:
  level: debug
  log_queries: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{EnvToken, EnvGateway, EnvDBURL, EnvDBType, EnvLogLevel, EnvConfig} {
		t.Setenv(k, "")
	}
	cfg, used, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if used != path || cfg.Token != "fp_live_file" || cfg.Security.ReadOnly || cfg.Security.MaxRowsLimit != 200 ||
		cfg.Security.QueryTimeoutMs != 3000 || cfg.Security.MaxConcurrent != 4 || cfg.Security.MaxResultBytes != 4_000_000 ||
		cfg.Gateway != DefaultGateway || cfg.Logging.Level != "debug" || !cfg.Logging.LogQueries || len(cfg.Writes.Allow) != 1 {
		t.Errorf("loaded: %+v", cfg)
	}

	t.Setenv(EnvToken, "fp_live_env")
	t.Setenv(EnvGateway, "ws://localhost:9000/v1/agent")
	t.Setenv(EnvLogLevel, "warn")
	cfg, _, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "fp_live_env" || cfg.Gateway != "ws://localhost:9000/v1/agent" || cfg.Logging.Level != "warn" {
		t.Errorf("env override: %+v", cfg)
	}
}

func TestEnvOnly(t *testing.T) {
	t.Setenv(EnvConfig, filepath.Join(t.TempDir(), "missing.yaml"))
	t.Setenv(EnvToken, "fp_live_env")
	t.Setenv(EnvDBURL, "mongodb://u:p@127.0.0.1:27017/qb")
	t.Setenv(EnvDBType, "")
	t.Setenv(EnvGateway, "")
	t.Setenv(EnvLogLevel, "")
	cfg, used, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if used != "" || cfg.Database.Type != "mongodb" || !cfg.Security.ReadOnly {
		t.Errorf("env only: used=%q %+v", used, cfg)
	}

	t.Setenv(EnvToken, "")
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), "setup") {
		t.Errorf("missing token should point at setup: %v", err)
	}
	if _, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("explicit missing file must fail")
	}
}

func TestDetectEngine(t *testing.T) {
	cases := map[string]string{
		"mongodb://x/y":             "mongodb",
		"mongodb+srv://x/y":         "mongodb",
		"mysql://u:p@h/d":           "mysql",
		"mariadb://u:p@h/d":         "mysql",
		"u:p@tcp(127.0.0.1:3306)/d": "mysql",
		"postgres://x":              "",
	}
	for url, want := range cases {
		if got := DetectEngine(url); got != want {
			t.Errorf("DetectEngine(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.Token = "t"
	cfg.Database.URL = "mysql://u:p@h/d"
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Gateway = "https://nope"
	if err := cfg.Validate(); err == nil {
		t.Error("gateway scheme")
	}
	cfg.Gateway = DefaultGateway
	cfg.Writes.Allow = []string{"system.users"}
	if err := cfg.Validate(); err == nil {
		t.Error("bad allow entry")
	}
}

func TestSaveIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.yaml")
	cfg := Default()
	cfg.Token = "fp_live_secret"
	cfg.Database.URL = "mysql://u:p@h/d"
	if err := Save(path, &cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "read_only: true") || !strings.Contains(string(data), "token: fp_live_secret") {
		t.Errorf("saved:\n%s", data)
	}
}
