// Package skill manages reusable system prompts (tenant-scoped).
package skill

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/utils"
)

// Skill is a named system prompt for text generation.
type Skill struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	SystemPrompt string     `json:"system_prompt"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

type CreateSkillRequest struct {
	Name         string `json:"name" binding:"required"`
	Description  string `json:"description"`
	SystemPrompt string `json:"system_prompt" binding:"required"`
}

type UpdateSkillRequest struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	SystemPrompt *string `json:"system_prompt"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists skills in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Create(skill *Skill) error {
	query := `INSERT INTO skills (id, name, description, system_prompt)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, skill.ID, skill.Name, skill.Description, skill.SystemPrompt).
		Scan(&skill.CreatedAt, &skill.UpdatedAt)
}

// GetByID returns one skill, or nil.
func (s *Store) GetByID(id string) (*Skill, error) {
	skill := &Skill{}
	err := s.db.QueryRow(`SELECT id, name, description, system_prompt, created_at, updated_at, deleted_at
		FROM skills WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&skill.ID, &skill.Name, &skill.Description, &skill.SystemPrompt,
			&skill.CreatedAt, &skill.UpdatedAt, &skill.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return skill, err
}

// List returns all skills.
func (s *Store) List() ([]Skill, error) {
	rows, err := s.db.Query(`SELECT id, name, description, system_prompt, created_at, updated_at, deleted_at
		FROM skills WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var skills []Skill
	for rows.Next() {
		var sk Skill
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.SystemPrompt,
			&sk.CreatedAt, &sk.UpdatedAt, &sk.DeletedAt); err != nil {
			return nil, err
		}
		skills = append(skills, sk)
	}
	return skills, rows.Err()
}

func (s *Store) Update(id string, updates map[string]interface{}) error {
	if len(updates) == 0 {
		return nil
	}
	query := "UPDATE skills SET updated_at = NOW()"
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
		return fmt.Errorf("skill not found")
	}
	return nil
}

func (s *Store) SoftDelete(id string) error {
	result, err := s.db.Exec(`UPDATE skills SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("skill not found")
	}
	return nil
}

// ─── Service ────────────────────────────────────────────────────

// Service implements skill business logic.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

// Create creates a skill.
func (s *Service) Create(req *CreateSkillRequest) (*Skill, error) {
	skill := &Skill{
		ID:           uuid.New().String(),
		Name:         req.Name,
		Description:  req.Description,
		SystemPrompt: req.SystemPrompt,
	}
	if err := s.store.Create(skill); err != nil {
		return nil, err
	}
	return skill, nil
}

// GetByID returns one skill.
func (s *Service) GetByID(id string) (*Skill, error) { return s.store.GetByID(id) }

// List returns all skills.
func (s *Service) List() ([]Skill, error) {
	skills, err := s.store.List()
	if err != nil {
		return nil, err
	}
	if skills == nil {
		skills = []Skill{}
	}
	return skills, nil
}

// Update updates a skill.
func (s *Service) Update(id string, req *UpdateSkillRequest) error {
	updates := map[string]interface{}{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.SystemPrompt != nil {
		updates["system_prompt"] = *req.SystemPrompt
	}
	return s.store.Update(id, updates)
}

// Delete soft-deletes a skill.
func (s *Service) Delete(id string) error { return s.store.SoftDelete(id) }

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes skill endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Create handles POST /skills
func (h *Handler) Create(c *gin.Context) {
	var req CreateSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	skill, err := h.svc.Create(&req)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, skill)
}

// List handles GET /skills
func (h *Handler) List(c *gin.Context) {
	skills, err := h.svc.List()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, skills)
}

// Get handles GET /skills/:id
func (h *Handler) Get(c *gin.Context) {
	skill, err := h.svc.GetByID(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if skill == nil {
		utils.NotFound(c, "skill not found")
		return
	}
	utils.Success(c, skill)
}

// Update handles PATCH /skills/:id
func (h *Handler) Update(c *gin.Context) {
	var req UpdateSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if err := h.svc.Update(c.Param("id"), &req); err != nil {
		if err.Error() == "skill not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	skill, err := h.svc.GetByID(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, skill)
}

// Delete handles DELETE /skills/:id
func (h *Handler) Delete(c *gin.Context) {
	if err := h.svc.Delete(c.Param("id")); err != nil {
		if err.Error() == "skill not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "skill deleted")
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers skill routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "skills" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/skills")
	g.Use(authMw, tenantMw)
	{
		g.POST("", m.priv("Create"))
		g.GET("", m.priv("List"))
		g.GET("/:id", m.priv("Get"))
		g.PATCH("/:id", m.priv("Update"))
		g.DELETE("/:id", m.priv("Delete"))
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
		case "Create":
			hdl.Create(c)
		case "List":
			hdl.List(c)
		case "Get":
			hdl.Get(c)
		case "Update":
			hdl.Update(c)
		case "Delete":
			hdl.Delete(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
