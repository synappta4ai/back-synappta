package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PoolConfig holds tuning applied to every connection pool.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open opens a database connection with the given URL and applies pool limits.
func Open(databaseURL string, pool PoolConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open db connection: %w", err)
	}

	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	return db, nil
}

// OpenWithSearchPath opens a database connection that sets search_path on every
// new connection via RuntimeParams. This is essential for
// schema-per-tenant multi-tenancy because the pgx driver ignores the
// options=-csearch_path connection parameter.
// The schema identifier is quoted here, so names with dashes (e.g.
// tenant_drako-inc) resolve as a single identifier instead of arithmetic.
func OpenWithSearchPath(databaseURL string, schema string, pool PoolConfig) (*sql.DB, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pgx config: %w", err)
	}

	quotedSchema, err := QuoteSchema(schema)
	if err != nil {
		return nil, err
	}

	// Set search_path as a runtime parameter — applied on every new connection
	if config.RuntimeParams == nil {
		config.RuntimeParams = make(map[string]string)
	}
	config.RuntimeParams["search_path"] = fmt.Sprintf("%s, public", quotedSchema)

	connStr := stdlib.RegisterConnConfig(config)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to open db connection: %w", err)
	}

	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping db: %w", err)
	}

	return db, nil
}

// Ping checks database liveness (used by /readyz).
func Ping(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// QuoteSchema validates and quotes a PostgreSQL schema identifier (e.g.
// tenant_drako-inc) for safe interpolation into SQL (CREATE SCHEMA,
// SET search_path, GUC values). Slugs allow dashes, so the raw name must
// never be interpolated unquoted.
func QuoteSchema(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty schema identifier")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", fmt.Errorf("invalid schema identifier %q: only [a-z0-9_-] allowed", name)
		}
	}
	if len(name) > 63 {
		return "", fmt.Errorf("schema identifier %q too long (max 63)", name)
	}
	return `"` + name + `"`, nil
}

// QuoteIdent validates and quotes a PostgreSQL identifier (schema or table name).
// Used when building dynamic schema-qualified SQL from tenant slugs.
func QuoteIdent(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty identifier")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "", fmt.Errorf("invalid identifier %q: only [a-z0-9_] allowed", name)
		}
	}
	if len(name) > 63 {
		return "", fmt.Errorf("identifier %q too long (max 63)", name)
	}
	return `"` + name + `"`, nil
}

// LogClose closes a pool logging any error (for deferred cleanup).
func LogClose(db *sql.DB, name string) {
	if err := db.Close(); err != nil {
		log.Printf("[db] error closing pool %s: %v", name, err)
	}
}
