// Package tenant exposes platform-admin endpoints to create and manage
// tenants (each backed by its own PostgreSQL schema).
package tenant

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/credential"
	"synapta/internal/modules/model"
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

func (s *Store) ByID(id int64) (*TenantView, error) {
	var t TenantView
	err := s.db.QueryRow(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

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
	reg         *tenancy.Registry
	encKey      string // credential encryption key (per-tenant stores)
}

func NewService(store *Store, provisioner *tenancy.Provisioner, reg *tenancy.Registry, encKey string) *Service {
	return &Service{store: store, provisioner: provisioner, reg: reg, encKey: encKey}
}

// TenantModelView is a catalog model enriched with the tenant's credential status.
type TenantModelView struct {
	model.Model
	CredentialConfigured bool `json:"credential_configured"`
}

// credentialStore opens the per-tenant credential store for a tenant view.
func (s *Service) credentialStore(t *TenantView) (*credential.Store, error) {
	pool, err := s.reg.Pool(t.Slug)
	if err != nil {
		return nil, fmt.Errorf("tenant pool for %q: %w", t.Slug, err)
	}
	return credential.NewStore(pool, s.encKey), nil
}

// ModelsForTenant returns the model catalog annotated with which credential
// providers the tenant already has configured.
func (s *Service) ModelsForTenant(id int64) ([]TenantModelView, error) {
	t, err := s.store.ByID(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("tenant %d not found", id)
	}

	configured := map[string]bool{}
	if pool, err := s.reg.Pool(t.Slug); err == nil {
		rows, err := pool.Query(`SELECT DISTINCT provider FROM credentials`)
		if err == nil {
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err == nil {
					configured[p] = true
				}
			}
			rows.Close()
		}
		// A missing/failing read only means "nothing configured".
	}

	catalog := model.List("")
	out := make([]TenantModelView, 0, len(catalog))
	for _, m := range catalog {
		out = append(out, TenantModelView{
			Model:                m,
			CredentialConfigured: configured[string(m.CredentialProvider)],
		})
	}
	return out, nil
}

// CredentialsForTenant lists the tenant's credentials (masked).
func (s *Service) CredentialsForTenant(id int64) ([]credential.Masked, error) {
	t, err := s.store.ByID(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("tenant %d not found", id)
	}
	store, err := s.credentialStore(t)
	if err != nil {
		return nil, err
	}
	return store.List()
}

// UpsertCredentialForTenant creates/updates one of the tenant's credentials.
func (s *Service) UpsertCredentialForTenant(id int64, req *credential.UpsertRequest) (*credential.Masked, error) {
	t, err := s.store.ByID(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("tenant %d not found", id)
	}
	store, err := s.credentialStore(t)
	if err != nil {
		return nil, err
	}
	cred, err := store.Upsert(req)
	if err != nil {
		return nil, err
	}
	return &credential.Masked{
		ID: cred.ID, Provider: cred.Provider, DisplayName: cred.DisplayName,
		Endpoint: cred.Endpoint, BaseURL: cred.BaseURL, Extra: cred.Extra,
		CreatedAt: cred.CreatedAt, UpdatedAt: cred.UpdatedAt,
	}, nil
}

// CredentialsExport is the file format for credential export/import.
// Secrets travel in PLAINTEXT — the file must be handled like the raw API
// keys it holds.
type CredentialsExport struct {
	Version     int                            `json:"version"`
	ExportedAt  time.Time                      `json:"exported_at"`
	Credentials []credential.ExportedCredential `json:"credentials"`
}

// ExportCredentialsForTenant returns the tenant's credentials with DECRYPTED
// secrets for backup/migration between environments. Platform superadmin only.
func (s *Service) ExportCredentialsForTenant(id int64) (*CredentialsExport, error) {
	t, err := s.store.ByID(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("tenant %d not found", id)
	}
	store, err := s.credentialStore(t)
	if err != nil {
		return nil, err
	}
	creds, err := store.ExportAll()
	if err != nil {
		return nil, err
	}
	return &CredentialsExport{
		Version:     1,
		ExportedAt:  time.Now().UTC(),
		Credentials: creds,
	}, nil
}

// ImportCredentialsForTenant upserts credentials from an import payload,
// overwriting existing entries for the same providers.
func (s *Service) ImportCredentialsForTenant(id int64, exp *CredentialsExport) (int, error) {
	t, err := s.store.ByID(id)
	if err != nil {
		return 0, err
	}
	if t == nil {
		return 0, fmt.Errorf("tenant %d not found", id)
	}
	store, err := s.credentialStore(t)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, ec := range exp.Credentials {
		if !credential.IsValidProvider(ec.Provider) {
			return imported, fmt.Errorf("invalid provider in import file: %q", ec.Provider)
		}
		_, err := store.Upsert(&credential.UpsertRequest{
			Provider:        ec.Provider,
			DisplayName:     ec.DisplayName,
			AccessKeyID:     ec.AccessKeyID,
			SecretAccessKey: ec.SecretAccessKey,
			APIKey:          ec.APIKey,
			Endpoint:        ec.Endpoint,
			BaseURL:         ec.BaseURL,
			Extra:           ec.Extra,
		})
		if err != nil {
			return imported, fmt.Errorf("failed to import credential for %q: %w", ec.Provider, err)
		}
		imported++
	}
	return imported, nil
}

// DeleteCredentialForTenant removes one of the tenant's credentials by provider.
func (s *Service) DeleteCredentialForTenant(id int64, provider string) error {
	t, err := s.store.ByID(id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("tenant %d not found", id)
	}
	store, err := s.credentialStore(t)
	if err != nil {
		return err
	}
	return store.Delete(provider)
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

// parseTenantID extracts the numeric :id param.
func parseTenantID(c *gin.Context) (int64, bool) {
	var id int64
	if _, err := fmt.Sscanf(c.Param("id"), "%d", &id); err != nil || id <= 0 {
		utils.BadRequest(c, "invalid tenant id")
		return 0, false
	}
	return id, true
}

// TenantModels handles GET /tenants/:id/models (platform superadmin).
// Returns the model catalog annotated with per-tenant credential status.
func (h *Handler) TenantModels(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	items, err := h.svc.ModelsForTenant(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, items)
}

// TenantCredentials handles GET /tenants/:id/credentials (platform superadmin).
func (h *Handler) TenantCredentials(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	items, err := h.svc.CredentialsForTenant(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if items == nil {
		items = []credential.Masked{}
	}
	utils.Success(c, items)
}

// TenantUpsertCredential handles PUT /tenants/:id/credentials (platform superadmin).
func (h *Handler) TenantUpsertCredential(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	var req credential.UpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	masked, err := h.svc.UpsertCredentialForTenant(id, &req)
	if err != nil {
		if contains(err.Error(), "invalid provider") {
			utils.BadRequest(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, masked)
}

// TenantDeleteCredential handles DELETE /tenants/:id/credentials/:provider (platform superadmin).
func (h *Handler) TenantDeleteCredential(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	provider := c.Param("provider")
	if !credential.IsValidProvider(provider) {
		utils.BadRequest(c, "invalid provider: "+provider)
		return
	}
	if err := h.svc.DeleteCredentialForTenant(id, provider); err != nil {
		if contains(err.Error(), "not found") {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "credential deleted")
}

// TenantExportCredentials handles GET /tenants/:id/credentials/export
// (platform superadmin). Returns a JSON file attachment with DECRYPTED
// secrets for backup/migration — treat the file like the raw keys it holds.
func (h *Handler) TenantExportCredentials(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	exp, err := h.svc.ExportCredentialsForTenant(id)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	filename := fmt.Sprintf("credentials-tenant-%d.json", id)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Data(200, "application/json", func() []byte {
		b, _ := json.MarshalIndent(exp, "", "  ")
		return b
	}())
}

// TenantImportCredentials handles POST /tenants/:id/credentials/import
// (platform superadmin). Accepts the JSON produced by the export endpoint
// (uploaded as multipart file or raw JSON body) and upserts credentials.
func (h *Handler) TenantImportCredentials(c *gin.Context) {
	id, ok := parseTenantID(c)
	if !ok {
		return
	}
	var exp CredentialsExport
	if fileHeader, err := c.FormFile("file"); err == nil {
		f, err := fileHeader.Open()
		if err != nil {
			utils.BadRequest(c, "cannot read uploaded file: "+err.Error())
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, 1<<20)) // 1 MiB cap
		if err != nil {
			utils.BadRequest(c, "cannot read uploaded file: "+err.Error())
			return
		}
		if err := json.Unmarshal(raw, &exp); err != nil {
			utils.BadRequest(c, "invalid import file: "+err.Error())
			return
		}
	} else {
		if err := c.ShouldBindJSON(&exp); err != nil {
			utils.BadRequest(c, "invalid import payload: "+err.Error())
			return
		}
	}
	if len(exp.Credentials) == 0 {
		utils.BadRequest(c, "import file has no credentials")
		return
	}
	imported, err := h.svc.ImportCredentialsForTenant(id, &exp)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, gin.H{"imported": imported})
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

// usersHdl serves the per-tenant user-management endpoints.
var usersHdl *TenantUsersHandler

// SetTenantUsersHandler wires the shared system DB into the users handlers.
// Called from main before routes are registered.
func SetTenantUsersHandler(h *TenantUsersHandler) { usersHdl = h }

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

		// Per-tenant user management (platform superadmin).
		if usersHdl != nil {
			g.GET("/platform-users", usersHdl.ListPlatformUsers)
			g.GET("/:id/users", usersHdl.List)
			g.POST("/:id/users", usersHdl.Create)
			g.PATCH("/:id/users/:userId/role", usersHdl.UpdateRole)
			g.PUT("/:id/users/:userId/permissions", usersHdl.UpdatePermissions)
			g.DELETE("/:id/users/:userId", usersHdl.Remove)
			g.GET("/permissions", func(c *gin.Context) {
				utils.Success(c, CatalogPermissions())
			})
		}

		// Per-tenant model/credential management (platform superadmin).
		g.GET("/:id/models", m.hdl.TenantModels)
		g.GET("/:id/credentials", m.hdl.TenantCredentials)
		g.PUT("/:id/credentials", m.hdl.TenantUpsertCredential)
		g.DELETE("/:id/credentials/:provider", m.hdl.TenantDeleteCredential)
		g.GET("/:id/credentials/export", m.hdl.TenantExportCredentials)
		g.POST("/:id/credentials/import", m.hdl.TenantImportCredentials)
	}
}
