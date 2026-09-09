package tenancy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
)

// Middleware resolves the tenant for every request.
//
// Resolution order:
//  1. JWT claim "tenant_id" → looked up in the system DB (must be active).
//  2. Header "X-Tenant-Slug" — only allowed for platform SUPER_ADMIN (level 0),
//     used by platform ops to impersonate a tenant.
//
// The resolved *Tenant is injected into the request context; handlers read it
// with tenancy.FromContext(c.Request.Context()).
func Middleware(reg *Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. JWT claim path (set by the auth middleware earlier in the chain).
		if tenantID, ok := c.Get("tenant_id"); ok {
			if id, ok := toInt64(tenantID); ok && id > 0 {
				t, err := tenantByID(reg, id)
				if err != nil {
					abort(c, 500, "failed to resolve tenant")
					return
				}
				if t == nil || !t.Active {
					abort(c, 403, "tenant not found or inactive")
					return
				}
				attach(c, t)
				return
			}
		}

		// 2. Header override — platform superadmin only.
		if slug := c.GetHeader("X-Tenant-Slug"); slug != "" {
			level := callerLevel(c)
			if level != 0 {
				abort(c, 403, "only platform superadmin may switch tenant via X-Tenant-Slug")
				return
			}
			t, err := tenantBySlug(reg, slug)
			if err != nil {
				abort(c, 500, "failed to resolve tenant")
				return
			}
			if t == nil || !t.Active {
				abort(c, 403, "tenant not found or inactive")
				return
			}
			attach(c, t)
			return
		}

		abort(c, 400, "missing tenant context: no tenant_id claim or X-Tenant-Slug header")
	}
}

func attach(c *gin.Context, t *Tenant) {
	ctx := Context(c.Request.Context(), t)
	c.Request = c.Request.WithContext(ctx)
	c.Set("tenant_slug", t.Slug)
	c.Next()
}

func abort(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "message": msg})
}

// callerLevel extracts role_level from the JWT claims (auth middleware).
func callerLevel(c *gin.Context) int {
	if raw, ok := c.Get("role_level"); ok {
		if f, ok := raw.(float64); ok {
			return int(f)
		}
	}
	return 99
}

func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

func tenantByID(reg *Registry, id int64) (*Tenant, error) {
	t := &Tenant{}
	err := reg.System().QueryRow(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func tenantBySlug(reg *Registry, slug string) (*Tenant, error) {
	if !ValidSlug(slug) {
		return nil, fmt.Errorf("invalid tenant slug")
	}
	t := &Tenant{}
	err := reg.System().QueryRow(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE slug = $1`, slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "no rows in result set") {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// ErrNoTenant is returned by helpers when the tenant context is missing.
var ErrNoTenant = errors.New("no tenant in context")

// RequireTenant is a helper for handlers: returns the tenant or sends an error.
func RequireTenant(c *gin.Context) (*Tenant, bool) {
	t := FromContext(c.Request.Context())
	if t == nil {
		c.AbortWithStatusJSON(400, gin.H{"success": false, "message": "missing tenant context"})
		return nil, false
	}
	return t, true
}
