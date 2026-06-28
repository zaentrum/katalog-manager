// Package store is the Postgres data-access layer over the existing
// com_nalet_katalog_* tables and katalogservice_* views. ALL identifiers are
// lowercase (Postgres folded the CAP-generated DDL). See SPEC §1.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New opens a pool against the given DSN. The DSN may be a postgres:// URL or a
// libpq keyword string; user/password override when non-empty (mirrors the CAP
// SPRING_DATASOURCE_USERNAME/PASSWORD split).
func New(ctx context.Context, dsn, user, password string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if user != "" {
		cfg.ConnConfig.User = user
	}
	if password != "" {
		cfg.ConnConfig.Password = password
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool for packages that need raw access
// (e.g. the REST layer's byte-range and ON CONFLICT statements).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies connectivity (used by the readiness probe).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
