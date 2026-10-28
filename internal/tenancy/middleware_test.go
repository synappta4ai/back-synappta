package tenancy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/db"
)

// stubConnector serves tenants from memory: the middleware only runs
// `SELECT ... FROM tenants WHERE id/slug = $1`, so the precedence rules can be
// tested without a Postgres instance.
type stubConnector struct{ tenants []*Tenant }

func (c stubConnector) Connect(context.Context) (driver.Conn, error) {
	return stubConn{tenants: c.tenants}, nil
}

func (c stubConnector) Driver() driver.Driver { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("stub driver: use the connector")
}

type stubConn struct{ tenants []*Tenant }

func (stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub conn: prepared statements not supported")
}

func (stubConn) Close() error { return nil }
func (stubConn) Begin() (driver.Tx, error) {
	return nil, errors.New("stub conn: transactions not supported")
}

func (c stubConn) QueryContext(
	_ context.Context,
	_ string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	if len(args) != 1 {
		return &stubRows{}, nil
	}
	for _, t := range c.tenants {
		switch key := args[0].Value.(type) {
		case int64:
			if t.ID == key {
				return &stubRows{tenant: t}, nil
			}
		case string:
			if t.Slug == key {
				return &stubRows{tenant: t}, nil
			}
		}
	}
	return &stubRows{}, nil
}

type stubRows struct {
	tenant *Tenant
	done   bool
}

func (r *stubRows) Columns() []string {
	return []string{"id", "slug", "name", "active", "created_at", "updated_at"}
}

func (r *stubRows) Close() error { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.tenant == nil || r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.tenant.ID
	dest[1] = r.tenant.Slug
	dest[2] = r.tenant.Name
	dest[3] = r.tenant.Active
	dest[4] = r.tenant.CreatedAt
	dest[5] = r.tenant.UpdatedAt
	return nil
}

func newStubRegistry(t *testing.T, tenants ...*Tenant) *Registry {
	t.Helper()
	sqlDB := sql.OpenDB(stubConnector{tenants: tenants})
	t.Cleanup(func() { sqlDB.Close() })
	return NewRegistry(sqlDB, "", db.PoolConfig{})
}

func activeTenant(id int64, slug string) *Tenant {
	now := time.Now()
	return &Tenant{ID: id, Slug: slug, Name: slug, Active: true, CreatedAt: now, UpdatedAt: now}
}

// TestMiddlewareTenantResolution pins the resolution order: the console's
// X-Tenant-Slug wins over the JWT tenant_id for a platform superadmin, while
// every other role keeps using the claim.
func TestMiddlewareTenantResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)

	inactive := activeTenant(3, "retired")
	inactive.Active = false

	reg := newStubRegistry(t, activeTenant(1, "synapta"), activeTenant(2, "acme"), inactive)

	tests := []struct {
		name     string
		level    float64
		claim    int64
		header   string
		want     int
		wantSlug string
	}{
		{name: "header overrides the claim for a superadmin", level: 0, claim: 1, header: "acme", want: 200, wantSlug: "acme"},
		{name: "claim resolves when there is no header", level: 0, claim: 1, want: 200, wantSlug: "synapta"},
		{name: "tenant admins cannot override the claim", level: 1, claim: 1, header: "acme", want: 403},
		{name: "unknown slug is rejected", level: 0, claim: 1, header: "ghost", want: 403},
		{name: "inactive tenant is rejected", level: 0, claim: 1, header: "retired", want: 403},
		{name: "no claim and no header is a bad request", level: 0, want: 400},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set("role_level", tc.level)
				if tc.claim > 0 {
					c.Set("tenant_id", tc.claim)
				}
			})
			r.Use(Middleware(reg))
			r.GET("/probe", func(c *gin.Context) {
				c.String(http.StatusOK, FromContext(c.Request.Context()).Slug)
			})

			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			if tc.header != "" {
				req.Header.Set(TenantHeader, tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if tc.wantSlug != "" && w.Body.String() != tc.wantSlug {
				t.Fatalf("tenant = %q, want %q", w.Body.String(), tc.wantSlug)
			}
		})
	}
}
