package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/compshare-agent/internal/config"
	// Historical exported names and YAML keys retain "MySQL" for deployment
	// compatibility; the driver and DSN are PostgreSQL.
	_ "github.com/lib/pq"
)

// OpenMySQL opens the PostgreSQL connection, configures the pool, pings the
// server, and verifies the schema. It closes the DB and returns an error on any
// failure. Its historical name is retained for compatibility.
func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
	dsn, err := ResolvePostgresDSN(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := VerifySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// ResolvePostgresDSN returns the exact DSN every PostgreSQL-backed component
// must use. Keeping host_override resolution in one exported function prevents
// secondary connections (for example the trace writer) from accidentally
// falling back to the unreachable host embedded in the base DSN.
func ResolvePostgresDSN(cfg config.MySQLConfig) (string, error) {
	if cfg.DSN == "" {
		return "", fmt.Errorf("database dsn is required")
	}
	return dsnWithHostOverride(cfg.DSN, cfg.HostOverride)
}

// dsnWithHostOverride preserves URL credentials, database, query parameters,
// and port while replacing only the reachable database host. IPv6 is rendered
// with the brackets required by PostgreSQL URL DSNs.
func dsnWithHostOverride(dsn, hostOverride string) (string, error) {
	host := strings.Trim(strings.TrimSpace(hostOverride), "[]")
	if host == "" {
		return dsn, nil
	}

	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse database dsn for host override: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("database host_override requires a postgres URL dsn")
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("database host_override requires a dsn host")
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	return u.String(), nil
}

// VerifySchema checks that all expected tables and columns exist by running
// no-op SELECTs. Column-level probes ensure a new binary started against an
// un-migrated database fails fast at boot instead of erroring on the first
// chat-path SQL (see deploy/migrations/README.md for the migration-first
// deploy contract).
func VerifySchema(ctx context.Context, db *sql.DB) error {
	queries := map[string]string{
		"sessions":                 "SELECT 1 FROM sessions LIMIT 0",
		"sessions.context_version": "SELECT context_version FROM sessions LIMIT 0",
		"messages":                 "SELECT 1 FROM messages LIMIT 0",
		"message_feedback":         "SELECT 1 FROM message_feedback LIMIT 0",
	}
	for target, q := range queries {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf("verify schema %s: %w", target, err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("verify schema %s close: %w", target, err)
		}
	}
	return nil
}

// VerifyTraceSchema checks that the optional HTTP trace table exists when the
// server is configured to persist traces to PostgreSQL.
func VerifyTraceSchema(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "SELECT 1 FROM agent_traces LIMIT 1").Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("verify schema table agent_traces: %w", err)
	}
	return nil
}
