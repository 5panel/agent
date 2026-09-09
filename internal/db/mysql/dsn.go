// Connection string handling for MySQL/MariaDB. Two spellings are
// accepted: a URL (mysql://user:pass@host:3306/db?charset=utf8mb4) and the
// Go driver's DSN (user:pass@tcp(host:3306)/db). Both end up as a
// mysql.Config; the agent never rebuilds a DSN string from it, so a
// password with "@" or "/" survives a round trip.
package mysql

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// ParseURL turns either spelling into a driver config with the settings the
// agent relies on: parseTime (dates arrive as time.Time, not text) and
// clientFoundRows (an update reports matched rows, like the other engine).
func ParseURL(raw string) (*mysql.Config, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("database url is empty")
	}
	var cfg *mysql.Config
	var err error
	if IsURL(raw) {
		cfg, err = fromURL(raw)
	} else {
		cfg, err = mysql.ParseDSN(raw)
	}
	if err != nil {
		return nil, err
	}
	if cfg.DBName == "" {
		return nil, errors.New("the MySQL url names no database (add /<database>)")
	}
	if cfg.Net == "" {
		cfg.Net = "tcp"
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:3306"
	}
	cfg.ParseTime = true
	cfg.ClientFoundRows = true
	return cfg, nil
}

// IsURL reports whether raw is the URL spelling.
func IsURL(raw string) bool {
	return strings.HasPrefix(raw, "mysql://") || strings.HasPrefix(raw, "mariadb://")
}

func fromURL(raw string) (*mysql.Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("bad mysql url: %w", err)
	}
	// The query string is handed to the driver's own parser so every DSN
	// parameter it knows (charset, tls, timeout, …) keeps working.
	cfg, err := mysql.ParseDSN("/" + strings.TrimPrefix(u.Path, "/") + "?" + u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("bad mysql url parameters: %w", err)
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	return cfg, nil
}

// Describe returns the host and database of a connection string for logs,
// never the credentials.
func Describe(raw string) (host, database string) {
	cfg, err := ParseURL(raw)
	if err != nil {
		return "?", "?"
	}
	return cfg.Addr, cfg.DBName
}
