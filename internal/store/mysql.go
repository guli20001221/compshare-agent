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
	// PostgreSQL driver, registered via blank import. The backing store is now
	// PostgreSQL; the OpenMySQL / MySQLConfig names are deliberately retained so
	// this swap stays surgical (server bootstrap + config keys unchanged). The
	// DSN is a libpq/URL string, e.g. postgresql://user:pass@host:5432/db?sslmode=disable.
	_ "github.com/lib/pq"
)

// OpenMySQL opens the PostgreSQL connection, configures the pool, pings the
// server, and verifies the schema. It closes the DB and returns an error on any
// failure. (Name retained for call-site stability; backend is PostgreSQL.)
func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("database dsn is required")
	}
	dsn, err := dsnWithHostOverride(cfg.DSN, cfg.HostOverride)
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
		"messages.turn_protocol":   "SELECT turn_id, turn_role FROM messages LIMIT 0",
		"message_feedback":         "SELECT 1 FROM message_feedback LIMIT 0",
		"chat_turns.contract": `SELECT id, session_id, top_organization_id, organization_id,
client_turn_id, turn_seq, request_hash, status, user_message_id, assistant_message_id,
base_context_version, committed_context_version, committed_lease_epoch, commit_hash,
error_code, executor_id, lease_epoch, has_external_action, execution_envelope,
retry_count, next_retry_at, next_event_seq, created_at,
updated_at, started_at, finished_at, committed_at FROM chat_turns LIMIT 0`,
		"conversation_leases.contract": `SELECT session_id, top_organization_id, organization_id,
active_turn_id, holder_id, lease_epoch, lease_until, created_at, updated_at
FROM conversation_leases LIMIT 0`,
		"turn_actions.contract": `SELECT turn_id, action_index, lease_epoch, action_name, args_hash,
execution_token, in_flight, upstream_request_id, status, result, error_code, context_hint, created_at, updated_at
FROM turn_actions LIMIT 0`,
		"chat_turn_events.contract": `SELECT turn_id, seq, lease_epoch, event_type, payload,
provisional, created_at FROM chat_turn_events LIMIT 0`,
		"turn_interactions.contract": `SELECT id, turn_id, interaction_key, kind, request_hash,
request_payload, lease_epoch, expires_at, status, resolution_hash, response_payload,
created_at, resolved_at, interaction_generation FROM turn_interactions LIMIT 0`,
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
