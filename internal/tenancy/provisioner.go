package tenancy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"synapta/internal/db"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

// Provisioner creates tenant schemas, runs tenant migrations and seeds rows.
type Provisioner struct {
	reg              *Registry
	tenantMigrations string // path to migrations/tenant
}

// NewProvisioner builds a provisioner over the registry.
func NewProvisioner(reg *Registry, tenantMigrations string) *Provisioner {
	return &Provisioner{reg: reg, tenantMigrations: tenantMigrations}
}

// CreateTenant creates the tenant row and provisions its schema atomically.
func (p *Provisioner) CreateTenant(name, slug string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("invalid tenant slug %q: must match [a-z0-9][a-z0-9_-]{1,62}", slug)
	}

	// Check uniqueness first (friendly error).
	var exists bool
	if err := p.reg.System().QueryRow(`SELECT EXISTS (SELECT 1 FROM tenants WHERE slug = $1)`, slug).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("tenant slug %q already exists", slug)
	}

	t := &Tenant{Slug: slug, Name: name, Active: true}
	err := p.reg.System().QueryRow(
		`INSERT INTO tenants (slug, name, active) VALUES ($1, $2, true) RETURNING id, created_at, updated_at`,
		slug, name,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("failed to insert tenant: %w", err)
	}

	if err := p.ProvisionSchema(slug); err != nil {
		// Roll back the row so we don't leave a tenant without schema.
		_, _ = p.reg.System().Exec(`DELETE FROM tenants WHERE id = $1`, t.ID)
		return nil, err
	}
	log.Printf("[provisioner] tenant %q created (schema %s)", slug, SchemaName(slug))
	return t, nil
}

// ProvisionSchema creates the schema and runs the tenant migration set on it.
// It is idempotent: existing schemas only get new migrations applied.
func (p *Provisioner) ProvisionSchema(slug string) error {
	schema := SchemaName(slug)
	if !ValidSlug(slug) {
		return fmt.Errorf("invalid tenant slug %q", slug)
	}
	// Los slugs admiten guion: el identificador debe ir comillado en SQL.
	quotedSchema, err := QuotedSchemaName(slug)
	if err != nil {
		return err
	}

	// Create schema if missing (idempotent).
	if _, err := p.reg.System().Exec(fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quotedSchema)); err != nil {
		return fmt.Errorf("failed to create schema %s: %w", schema, err)
	}

	// Open a short-lived connection pinned to the schema and run goose there.
	// Using OpenWithSearchPath to set search_path via pgx AfterConnect hook
	// because pgx ignores the options=-csearch_path parameter.
	sysURL, err := p.reg.SystemDSN()
	if err != nil {
		return err
	}
	pool, err := db.OpenWithSearchPath(sysURL, schema, db.PoolConfig{
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	})
	if err != nil {
		return fmt.Errorf("failed to open migration connection for %s: %w", schema, err)
	}
	defer pool.Close()

	// Explicitly set search_path so goose migrations resolve unqualified
	// table names (e.g. preset_groups) inside the tenant schema. The
	// RuntimeParams set via OpenWithSearchPath are applied by pgx on new
	// connections, but goose's internal connection handling may not inherit
	// them reliably.
	//
	// NOTE: We intentionally omit "public" from the search path. The
	// system migration creates goose_db_version in public; including
	// public would let goose find that version=1 record and skip
	// 00001_core.sql entirely for every tenant.
	if _, err := pool.Exec(fmt.Sprintf("SET search_path TO %s", quotedSchema)); err != nil {
		return fmt.Errorf("failed to set search_path for %s: %w", schema, err)
	}

	if err := goose.Up(pool, p.tenantMigrations); err != nil {
		return fmt.Errorf("tenant migrations failed for schema %s: %w", schema, err)
	}

	// Seed per-tenant roles (roles table lives in every tenant schema).
	if err := seedTenantRoles(pool); err != nil {
		return fmt.Errorf("failed to seed roles for %s: %w", schema, err)
	}

	// Invalidate any cached pool so it re-opens with fresh schema knowledge.
	p.reg.ClosePool(slug)
	return nil
}

// MigrateAllTenants runs tenant migrations on every existing tenant schema.
// Called at startup so schema changes roll out to all tenants.
func (p *Provisioner) MigrateAllTenants() error {
	schemas, err := p.reg.ListTenantSchemas()
	if err != nil {
		return fmt.Errorf("failed to list tenant schemas: %w", err)
	}
	for _, schema := range schemas {
		slug := trimPrefix(schema, "tenant_")
		if !ValidSlug(slug) {
			log.Printf("[provisioner] skipping non-tenant schema %q", schema)
			continue
		}
		if err := p.ProvisionSchema(slug); err != nil {
			return fmt.Errorf("migrating %s: %w", schema, err)
		}
		log.Printf("[provisioner] tenant schema %s up to date", schema)
	}
	return nil
}

// DeactivateTenant marks the tenant inactive and closes its pool.
func (p *Provisioner) DeactivateTenant(id int64) error {
	if _, err := p.reg.System().Exec(`UPDATE tenants SET active = false, updated_at = NOW() WHERE id = $1`, id); err != nil {
		return err
	}
	var slug string
	if err := p.reg.System().QueryRow(`SELECT slug FROM tenants WHERE id = $1`, id).Scan(&slug); err == nil && slug != "" {
		p.reg.ClosePool(slug)
	}
	return nil
}

// EnsureMembership grants a user a role inside a tenant.
func (p *Provisioner) EnsureMembership(tenantID, userID int64, roleLevel int) error {
	_, err := p.reg.System().Exec(`
		INSERT INTO tenant_memberships (tenant_id, user_id, role_level)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET role_level = EXCLUDED.role_level, updated_at = NOW()`,
		tenantID, userID, roleLevel)
	return err
}

// MembershipRoleLevel returns the user's role level inside a tenant, or -1.
func (p *Provisioner) MembershipRoleLevel(tenantID, userID int64) (int, error) {
	var level int
	err := p.reg.System().QueryRow(
		`SELECT role_level FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID,
	).Scan(&level)
	if err == sql.ErrNoRows {
		return -1, nil
	}
	return level, err
}

// MembershipsForUser returns every active membership (tenant, role,
// effective permissions). Effective = permisos de la empresa (tenants.permissions)
// ∪ extras de la membresía, para que los cambios a nivel de empresa alcancen
// a todos sus usuarios.
func (p *Provisioner) MembershipsForUser(userID int64) ([]Membership, error) {
	rows, err := p.reg.System().Query(`
		SELECT m.tenant_id, m.role_level,
		       to_jsonb(COALESCE(m.permissions, '{}'::text[]))::text,
		       to_jsonb(COALESCE(t.permissions, '{}'::text[]))::text
		FROM tenant_memberships m
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.active = true`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Membership
	for rows.Next() {
		var m Membership
		var memberJSON, tenantJSON string
		if err := rows.Scan(&m.TenantID, &m.RoleLevel, &memberJSON, &tenantJSON); err != nil {
			return nil, err
		}
		m.Permissions = unionPerms(parsePermsJSON(memberJSON), parsePermsJSON(tenantJSON))
		out = append(out, m)
	}
	return out, rows.Err()
}

// unionPerms merges two permission sets; '*' gana sobre todo lo demás.
func unionPerms(sets ...[]string) []string {
	for _, set := range sets {
		for _, p := range set {
			if p == "*" {
				return []string{"*"}
			}
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, set := range sets {
		for _, p := range set {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// parsePermsJSON parses a JSON array of strings (to_jsonb of a text[]);
// database/sql no escanea text[] directo a []string.
func parsePermsJSON(raw string) []string {
	var perms []string
	if raw == "" || raw == "[]" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &perms); err != nil || perms == nil {
		return []string{}
	}
	return perms
}

// TenantIDBySlug resolves a slug to its tenant id, or 0 when missing.
func (p *Provisioner) TenantIDBySlug(slug string) (int64, error) {
	var id int64
	err := p.reg.System().QueryRow(`SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// seedTenantRoles mirrors the dcs-back role ladder inside each tenant schema.
func seedTenantRoles(dbh *sql.DB) error {
	_, err := dbh.Exec(`
		INSERT INTO roles (id, name, level) VALUES
			(0, 'SUPER_ADMIN', 0),
			(1, 'ADMIN', 1),
			(2, 'DIRECTOR', 2),
			(3, 'USER', 3)
		ON CONFLICT (level) DO NOTHING`)
	return err
}

// ProvisionToken generates an idempotency token for provision API responses.
func ProvisionToken() string { return uuid.New().String() }

// EnsureDefaultTenant guarantees the default tenant (slug) exists with its
// schema provisioned and grants the superadmin a membership in it. Safe to
// call on every boot: it is fully idempotent.
func (p *Provisioner) EnsureDefaultTenant(slug, name string, superadminID int64) error {
	if !ValidSlug(slug) {
		return fmt.Errorf("invalid default tenant slug %q", slug)
	}

	var id int64
	err := p.reg.System().QueryRow(`SELECT id FROM tenants WHERE slug = $1`, slug).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		t, err := p.CreateTenant(name, slug)
		if err != nil {
			return fmt.Errorf("failed to create default tenant %q: %w", slug, err)
		}
		id = t.ID
		log.Printf("[provisioner] default tenant %q created (id=%d)", slug, id)
	} else if err != nil {
		return err
	}

	if superadminID > 0 {
		if err := p.EnsureMembership(id, superadminID, 0); err != nil {
			return fmt.Errorf("failed to add superadmin to default tenant: %w", err)
		}
	}
	return nil
}

// MigrateSystem runs the system (public schema) migrations. Exposed so main
// only depends on the provisioner for all migration concerns.
func (p *Provisioner) MigrateSystem(systemMigrations string) error {
	if err := goose.Up(p.reg.System(), systemMigrations); err != nil {
		return fmt.Errorf("system migrations failed: %w", err)
	}
	return nil
}

// PingSystem checks the system DB (readiness).
func (p *Provisioner) PingSystem(ctx context.Context) error {
	return p.reg.System().PingContext(ctx)
}

// trimPrefix removes prefix from s (strings.TrimPrefix helper for clarity).
func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}
