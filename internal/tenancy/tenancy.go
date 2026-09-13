// Package tenancy implements schema-per-tenant multi-tenancy on PostgreSQL.
//
// Layout:
//   - public schema: system tables (tenants, users, tenant_memberships)
//   - tenant_<slug> schema per tenant: all domain tables
//
// Each tenant gets a lazily-created connection pool cached in the Registry.
// Tenant-scoped queries run against the tenant's own schema; the tenant
// context is injected by the middleware and carried through a context key.
package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"synapta/internal/db"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}$`)

// ValidSlug reports whether a tenant slug is well-formed.
func ValidSlug(slug string) bool { return slugRe.MatchString(slug) }

// SchemaName maps a tenant slug to its PostgreSQL schema name.
func SchemaName(slug string) string { return "tenant_" + slug }

type ctxKey int

const tenantKey ctxKey = iota

// Tenant is a platform tenant (one event agency / organization).
type Tenant struct {
	ID        int64
	Slug      string
	Name      string
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Context injects a tenant into a request context.
func Context(ctx context.Context, t *Tenant) context.Context {
	return context.WithValue(ctx, tenantKey, t)
}

// FromContext returns the tenant in ctx, or nil when absent.
func FromContext(ctx context.Context) *Tenant {
	if t, ok := ctx.Value(tenantKey).(*Tenant); ok {
		return t
	}
	return nil
}

// FromContextSlug returns the tenant slug in ctx, or "".
func FromContextSlug(ctx context.Context) string {
	if t := FromContext(ctx); t != nil {
		return t.Slug
	}
	return ""
}

// Registry owns the system connection and the per-tenant pool cache.
type Registry struct {
	systemDB  *sql.DB
	systemURL string // raw DSN used to build tenant DSNs
	poolCfg   db.PoolConfig

	mu    sync.RWMutex
	pools map[string]*sql.DB // slug → pool
}

// NewRegistry creates a registry over the system (public schema) connection.
// systemURL is the raw DSN (used to derive tenant DSNs with pinned search_path).
func NewRegistry(systemDB *sql.DB, systemURL string, poolCfg db.PoolConfig) *Registry {
	return &Registry{
		systemDB:  systemDB,
		systemURL: systemURL,
		poolCfg:   poolCfg,
		pools:     map[string]*sql.DB{},
	}
}

// System returns the shared system-schema connection.
func (r *Registry) System() *sql.DB { return r.systemDB }

// Pool returns (creating and caching if needed) the tenant's dedicated pool.
func (r *Registry) Pool(slug string) (*sql.DB, error) {
	r.mu.RLock()
	p, ok := r.pools[slug]
	r.mu.RUnlock()
	if ok {
		return p, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-checked locking.
	if p, ok := r.pools[slug]; ok {
		return p, nil
	}

	// Verify the schema actually exists before opening a pool to it.
	exists, err := r.SchemaExists(slug)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("tenant schema for %q does not exist", slug)
	}

	// Connect with search_path pinned to the tenant schema using AfterConnect hook
	// because pgx ignores the options=-csearch_path parameter.
	sysURL, err := r.SystemDSN()
	if err != nil {
		return nil, err
	}
	p, err = db.OpenWithSearchPath(sysURL, SchemaName(slug), r.poolCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open tenant pool for %q: %w", slug, err)
	}
	r.pools[slug] = p
	log.Printf("[tenancy] opened pool for tenant %q", slug)
	return p, nil
}

// ClosePool closes and forgets the tenant pool (used when a tenant is deactivated).
func (r *Registry) ClosePool(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pools[slug]; ok {
		db.LogClose(p, "tenant "+slug)
		delete(r.pools, slug)
	}
}

// CloseAll closes every cached pool (graceful shutdown).
func (r *Registry) CloseAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for slug, p := range r.pools {
		db.LogClose(p, "tenant "+slug)
		delete(r.pools, slug)
	}
}

// SystemDSN returns the system URL with search_path forced to public.
func (r *Registry) SystemDSN() (string, error) {
	if r.systemDB == nil {
		return "", errors.New("system db not initialized")
	}
	url := r.systemURL
	if url == "" {
		return "", errors.New("system DSN not set")
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "sslmode=disable", nil
}

// SchemaExists reports whether the tenant schema exists.
func (r *Registry) SchemaExists(slug string) (bool, error) {
	var exists bool
	err := r.systemDB.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		SchemaName(slug),
	).Scan(&exists)
	return exists, err
}

// TenantBySlug returns the tenant row for a slug, or nil when missing.
func (r *Registry) TenantBySlug(slug string) (*Tenant, error) {
	t := &Tenant{}
	err := r.systemDB.QueryRow(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE slug = $1`, slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListTenantSchemas returns all existing tenant_* schema names.
func (r *Registry) ListTenantSchemas() ([]string, error) {
	rows, err := r.systemDB.Query(
		`SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'tenant_%' ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListActiveTenants returns all active tenants from the system schema.
func (r *Registry) ListActiveTenants() ([]Tenant, error) {
	rows, err := r.systemDB.Query(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE active = true ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
