package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/compshare-agent/internal/config"
	_ "github.com/go-sql-driver/mysql"
)

// OpenMySQL opens a MySQL connection, configures the pool, pings the server,
// and verifies the schema. It closes the DB and returns an error on any failure.
func OpenMySQL(cfg config.MySQLConfig) (*sql.DB, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("mysql dsn is required")
	}
	db, err := sql.Open("mysql", cfg.DSN)
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
	for _, table := range []string{"sessions", "messages", "message_feedback"} {
		query := fmt.Sprintf("SELECT 1 FROM %s LIMIT 1", table) //nolint:gosec
		if _, err := db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("verify schema table %s: %w", table, err)
		}
	}
	return nil
}
