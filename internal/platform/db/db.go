// Package db provides the Postgres connection pool.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open creates a *sql.DB from a DSN and pool settings, verifying connectivity.
func Open(ctx context.Context, dsn string, maxOpen, maxIdle int, connMaxLifetime time.Duration) (*sql.DB, error) {
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	d.SetMaxOpenConns(maxOpen)
	d.SetMaxIdleConns(maxIdle)
	d.SetConnMaxLifetime(connMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := d.PingContext(pingCtx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return d, nil
}
