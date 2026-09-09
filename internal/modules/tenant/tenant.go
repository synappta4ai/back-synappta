// Package tenant exposes platform-admin endpoints to create and manage
// tenants (each backed by its own PostgreSQL schema).
package tenant

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/tenancy"
	"synapta/internal/utils"
)

// TenantView is the API representation of a tenant.
type TenantView struct {
	ID        int64     `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store reads/writes tenant rows in the system schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) List() ([]TenantView, error) {
	rows, err := s.db.Query(`SELECT id, slug, name, active, created_at, updated_at FROM tenants ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantView
	for rows.Next() {
		var t TenantView
		if err := rows.Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Service orchestrates tenant lifecycle via the tenancy provisioner.
type Service struct {
	store       *Store
	provisioner *tenancy.Provisioner
}

func NewService(store *Store, provisioner *tenancy.Provisioner) *Service {
	return &Service{store: store, provisioner: provisioner}
}

// Create provisions a new tenant end-to-end.
func (s *Service) Create(name, slug string) (*TenantView, error) {
	t, err := s.provisioner.CreateTenant(name, slug)
	if err != nil {
		return nil, err
	}
	return &TenantView{ID: t.ID, Slug: t.Slug, Name: t.Name, Active: t.Active, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}, nil
}

// List returns all tenants.
func (s *Service) List() ([]TenantView, error) { return s.store.List() }

// Deactivate disables a tenant and closes its pool.
func (s *Service) Deactivate(id int64) error { return s.provisioner.DeactivateTenant(id) }

// Handler exposes tenant endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// List handles GET /tenants (platform superadmin)
func (h *Handler) List(c *gin.Context) {
	items, err := h.svc.List()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if items == nil {
		items = []TenantView{}
	}
	utils.Success(c, items)
}

// Create handles POST /tenants (platform superadmin)
func (h *Handler) Create(c *gin.Context) {
	var req struct {
		Name string `json:"name" binding:"required"`
		Slug string `json:"slug" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	t, err := h.svc.Create(req.Name, req.Slug)
	if err != nil {
		if errors.Is(err, tenancy.ErrNoTenant) || contains(err.Error(), "already exists") || contains(err.Error(), "invalid tenant slug") {
			utils.Conflict(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, t)
}

// Deactivate handles PATCH /tenants/:id/deactivate (platform superadmin)
func (h *Handler) Deactivate(c *gin.Context) {
	var id int64
	if _, err := fmt.Sscanf(c.Param("id"), "%d", &id); err != nil {
		utils.BadRequest(c, "invalid tenant id")
		return
	}
	if err := h.svc.Deactivate(id); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "tenant deactivated")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Module registers tenant admin routes (platform superadmin only).
type Module struct {
	hdl *Handler
}

func NewModule(hdl *Handler) *Module { return &Module{hdl: hdl} }

func (m *Module) Name() string { return "tenants" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, _, _ gin.HandlerFunc) {
	g := rg.Group("/tenants")
	g.Use(authMw, gin.HandlerFunc(func(c *gin.Context) {
		// Platform superadmin only (role_level == 0).
		raw, _ := c.Get("role_level")
		level, ok := raw.(float64)
		if !ok || int(level) != 0 {
			c.AbortWithStatusJSON(403, gin.H{"success": false, "message": "platform superadmin only"})
			return
		}
		c.Next()
	}))
	{
		g.GET("", m.hdl.List)
		g.POST("", m.hdl.Create)
		g.PATCH("/:id/deactivate", m.hdl.Deactivate)
	}
}
