package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/go-sql-driver/mysql"
)

// OpenMySQL opens a MySQL connection, configures the pool, pings the server,
// and verifies the schema. It closes the DB and returns an error on any failure.
func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("mysql dsn is required")
	}
	parsed, err := mysql.ParseDSN(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse mysql dsn: %w", err)
	}
	parsed.ParseTime = true
	parsed.Loc = time.UTC
	dsn := parsed.FormatDSN()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	if err := VerifySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// VerifySchema checks that all expected tables exist by running a no-op SELECT.
func VerifySchema(ctx context.Context, db *sql.DB) error {
	queries := map[string]string{
		"sessions":         "SELECT 1 FROM sessions LIMIT 1",
		"messages":         "SELECT 1 FROM messages LIMIT 1",
		"message_feedback": "SELECT 1 FROM message_feedback LIMIT 1",
	}
	for table, q := range queries {
		var v int
		if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("verify schema table %s: %w", table, err)
		}
	}
	return nil
}
