// Package preset manages prompt preset groups and presets (tenant-scoped).
package preset

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/middleware"
	"synapta/internal/utils"
)

// ─── Types ──────────────────────────────────────────────────────

// Group is a preset category (lens, camera, grading...).
type Group struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Description string     `json:"description,omitempty"`
	Active      bool       `json:"active"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

// Preset is one reusable prompt fragment.
type Preset struct {
	ID        string     `json:"id"`
	GroupID   string     `json:"group_id"`
	Code      string     `json:"code"`
	Label     string     `json:"label"`
	LabelKey  string     `json:"label_key"`
	Prompt    string     `json:"prompt"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type CreateGroupRequest struct {
	Name        string `json:"name" binding:"required"`
	Slug        string `json:"slug" binding:"required"`
	Description string `json:"description"`
}

type UpdateGroupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Active      *bool   `json:"active"`
}

type CreatePresetRequest struct {
	GroupID  string `json:"group_id" binding:"required"`
	Code     string `json:"code" binding:"required"`
	Label    string `json:"label" binding:"required"`
	LabelKey string `json:"label_key"`
	Prompt   string `json:"prompt" binding:"required"`
}

type UpdatePresetRequest struct {
	Code   *string `json:"code"`
	Label  *string `json:"label"`
	Prompt *string `json:"prompt"`
	Active *bool   `json:"active"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists presets in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const groupCols = `id, name, slug, COALESCE(description,'') AS description, active, created_at, updated_at, deleted_at`

// ListGroups returns all groups, optionally including inactive ones.
func (s *Store) ListGroups(includeInactive bool) ([]Group, error) {
	where := "WHERE deleted_at IS NULL"
	if !includeInactive {
		where += " AND active = true"
	}
	rows, err := s.db.Query(`SELECT ` + groupCols + ` FROM preset_groups ` + where + ` ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Slug, &g.Description, &g.Active,
			&g.CreatedAt, &g.UpdatedAt, &g.DeletedAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *Store) CreateGroup(g *Group) error {
	query := `INSERT INTO preset_groups (id, name, slug, description, active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, g.ID, g.Name, g.Slug, nullIfEmpty(g.Description), g.Active).
		Scan(&g.CreatedAt, &g.UpdatedAt)
}

func (s *Store) UpdateGroup(id string, updates map[string]interface{}) error {
	if len(updates) == 0 {
		return nil
	}
	query := "UPDATE preset_groups SET updated_at = NOW()"
	args := []interface{}{}
	argIdx := 1
	for col, val := range updates {
		query += fmt.Sprintf(", %s = $%d", col, argIdx)
		args = append(args, val)
		argIdx++
	}
	query += fmt.Sprintf(" WHERE id = $%d AND deleted_at IS NULL", argIdx)
	args = append(args, id)

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("group not found")
	}
	return nil
}

const presetCols = `id, group_id, code, label, COALESCE(label_key,'') AS label_key, prompt, active, created_at, updated_at, deleted_at`

// ListPresets returns presets, optionally filtered by group.
func (s *Store) ListPresets(groupID string, includeInactive bool) ([]Preset, error) {
	where := "WHERE deleted_at IS NULL"
	args := []interface{}{}
	argIdx := 1
	if groupID != "" {
		where += fmt.Sprintf(" AND group_id = $%d", argIdx)
		args = append(args, groupID)
		argIdx++
	}
	if !includeInactive {
		where += " AND active = true"
	}
	rows, err := s.db.Query(`SELECT `+presetCols+` FROM presets `+where+` ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var presets []Preset
	for rows.Next() {
		var p Preset
		if err := rows.Scan(&p.ID, &p.GroupID, &p.Code, &p.Label, &p.LabelKey, &p.Prompt,
			&p.Active, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
			return nil, err
		}
		presets = append(presets, p)
	}
	return presets, rows.Err()
}

// GetPresetByID returns one preset, or nil.
func (s *Store) GetPresetByID(id string) (*Preset, error) {
	p := &Preset{}
	err := s.db.QueryRow(`SELECT `+presetCols+` FROM presets WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&p.ID, &p.GroupID, &p.Code, &p.Label, &p.LabelKey, &p.Prompt, &p.Active,
			&p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (s *Store) CreatePreset(p *Preset) error {
	query := `INSERT INTO presets (id, group_id, code, label, label_key, prompt, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, p.ID, p.GroupID, p.Code, p.Label, p.LabelKey, p.Prompt, p.Active).
		Scan(&p.CreatedAt, &p.UpdatedAt)
}

func (s *Store) UpdatePreset(id string, updates map[string]interface{}) error {
	if len(updates) == 0 {
		return nil
	}
	query := "UPDATE presets SET updated_at = NOW()"
	args := []interface{}{}
	argIdx := 1
	for col, val := range updates {
		query += fmt.Sprintf(", %s = $%d", col, argIdx)
		args = append(args, val)
		argIdx++
	}
	query += fmt.Sprintf(" WHERE id = $%d AND deleted_at IS NULL", argIdx)
	args = append(args, id)

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("preset not found")
	}
	return nil
}

func (s *Store) SoftDeletePreset(id string) error {
	result, err := s.db.Exec(`UPDATE presets SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("preset not found")
	}
	return nil
}

func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ─── Service ────────────────────────────────────────────────────

// Service implements preset business logic.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

// ListGroups returns groups.
func (s *Service) ListGroups(includeInactive bool) ([]Group, error) {
	groups, err := s.store.ListGroups(includeInactive)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

// CreateGroup creates a preset group.
func (s *Service) CreateGroup(req *CreateGroupRequest) (*Group, error) {
	g := &Group{ID: uuid.New().String(), Name: req.Name, Slug: req.Slug,
		Description: req.Description, Active: true}
	if err := s.store.CreateGroup(g); err != nil {
		return nil, err
	}
	return g, nil
}

// UpdateGroup updates a preset group.
func (s *Service) UpdateGroup(id string, req *UpdateGroupRequest) (*Group, error) {
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if err := s.store.UpdateGroup(id, updates); err != nil {
		return nil, err
	}
	return s.GetGroupByID(id)
}

// GetGroupByID re-fetches a group after update.
func (s *Service) GetGroupByID(id string) (*Group, error) {
	groups, err := s.store.ListGroups(true)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.ID == id {
			return &g, nil
		}
	}
	return nil, fmt.Errorf("group not found")
}

// ListPresets returns presets.
func (s *Service) ListPresets(groupID string, includeInactive bool) ([]Preset, error) {
	presets, err := s.store.ListPresets(groupID, includeInactive)
	if err != nil {
		return nil, err
	}
	if presets == nil {
		presets = []Preset{}
	}
	return presets, nil
}

// GetPresetByID returns one preset.
func (s *Service) GetPresetByID(id string) (*Preset, error) { return s.store.GetPresetByID(id) }

// CreatePreset creates a preset.
func (s *Service) CreatePreset(req *CreatePresetRequest) (*Preset, error) {
	p := &Preset{ID: uuid.New().String(), GroupID: req.GroupID, Code: req.Code,
		Label: req.Label, LabelKey: req.LabelKey, Prompt: req.Prompt, Active: true}
	if err := s.store.CreatePreset(p); err != nil {
		return nil, err
	}
	return p, nil
}

// UpdatePreset updates a preset.
func (s *Service) UpdatePreset(id string, req *UpdatePresetRequest) (*Preset, error) {
	updates := map[string]interface{}{}
	if req.Code != nil {
		updates["code"] = *req.Code
	}
	if req.Label != nil {
		updates["label"] = *req.Label
	}
	if req.Prompt != nil {
		updates["prompt"] = *req.Prompt
	}
	if req.Active != nil {
		updates["active"] = *req.Active
	}
	if err := s.store.UpdatePreset(id, updates); err != nil {
		return nil, err
	}
	return s.store.GetPresetByID(id)
}

// SoftDeletePreset soft-deletes a preset.
func (s *Service) SoftDeletePreset(id string) error { return s.store.SoftDeletePreset(id) }

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes preset endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ListGroups handles GET /presets/groups
func (h *Handler) ListGroups(c *gin.Context) {
	groups, err := h.svc.ListGroups(c.Query("include_inactive") == "true")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, groups)
}

// CreateGroup handles POST /presets/groups
func (h *Handler) CreateGroup(c *gin.Context) {
	var req CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.svc.CreateGroup(&req)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, result)
}

// UpdateGroup handles PATCH /presets/groups/:id
func (h *Handler) UpdateGroup(c *gin.Context) {
	var req UpdateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.svc.UpdateGroup(c.Param("id"), &req)
	if err != nil {
		if err.Error() == "group not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// ListPresets handles GET /presets
func (h *Handler) ListPresets(c *gin.Context) {
	presets, err := h.svc.ListPresets(c.Query("group_id"), c.Query("include_inactive") == "true")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, presets)
}

// GetPreset handles GET /presets/:id
func (h *Handler) GetPreset(c *gin.Context) {
	result, err := h.svc.GetPresetByID(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if result == nil {
		utils.NotFound(c, "preset not found")
		return
	}
	utils.Success(c, result)
}

// CreatePreset handles POST /presets
func (h *Handler) CreatePreset(c *gin.Context) {
	var req CreatePresetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.svc.CreatePreset(&req)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, result)
}

// UpdatePreset handles PATCH /presets/:id
func (h *Handler) UpdatePreset(c *gin.Context) {
	var req UpdatePresetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.svc.UpdatePreset(c.Param("id"), &req)
	if err != nil {
		if err.Error() == "preset not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// SoftDeletePreset handles DELETE /presets/:id
func (h *Handler) SoftDeletePreset(c *gin.Context) {
	if err := h.svc.SoftDeletePreset(c.Param("id")); err != nil {
		if err.Error() == "preset not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "preset deleted")
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers preset routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "presets" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/presets")
	g.Use(authMw, tenantMw)
	{
		g.GET("/groups", m.priv("ListGroups"))
		g.POST("/groups", middleware.RequireRole(2), m.priv("CreateGroup"))
		g.PATCH("/groups/:id", middleware.RequireRole(2), m.priv("UpdateGroup"))

		g.GET("", m.priv("ListPresets"))
		g.GET("/:id", m.priv("GetPreset"))
		g.POST("", middleware.RequireRole(2), m.priv("CreatePreset"))
		g.PATCH("/:id", middleware.RequireRole(2), m.priv("UpdatePreset"))
		g.DELETE("/:id", middleware.RequireRole(2), m.priv("SoftDeletePreset"))
	}
}

// priv resolves the tenant handler then dispatches to the named method.
func (m *Module) priv(method string) gin.HandlerFunc {
	return func(c *gin.Context) {
		hdl, ok := m.resolve(c)
		if !ok {
			return
		}
		switch method {
		case "ListGroups":
			hdl.ListGroups(c)
		case "CreateGroup":
			hdl.CreateGroup(c)
		case "UpdateGroup":
			hdl.UpdateGroup(c)
		case "ListPresets":
			hdl.ListPresets(c)
		case "GetPreset":
			hdl.GetPreset(c)
		case "CreatePreset":
			hdl.CreatePreset(c)
		case "UpdatePreset":
			hdl.UpdatePreset(c)
		case "SoftDeletePreset":
			hdl.SoftDeletePreset(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
