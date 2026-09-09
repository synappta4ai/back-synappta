// Package assignment implements the agnostic, polymorphic resource
// attachment table: any resource (ingredient, file, preset, skill, model)
// attaches to any assignable element (event, program, piece — extensible).
package assignment

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/utils"
)

// Assignable element types (extensible; validated on write).
const (
	TargetEvent   = "event"
	TargetProgram = "program"
	TargetPiece   = "piece"
)

// Resource types that can be attached.
const (
	ResourceIngredient = "ingredient"
	ResourceFile       = "file"
	ResourcePreset     = "preset"
	ResourceSkill      = "skill"
	ResourceModel      = "model"
)

var validTargets = map[string]bool{
	TargetEvent: true, TargetProgram: true, TargetPiece: true,
}

var validResources = map[string]bool{
	ResourceIngredient: true, ResourceFile: true, ResourcePreset: true,
	ResourceSkill: true, ResourceModel: true,
}

// ValidTarget reports whether the assignable type is supported.
func ValidTarget(t string) bool { return validTargets[t] }

// ValidResource reports whether the resource type is supported.
func ValidResource(r string) bool { return validResources[r] }

// Assignment links one resource to one target element with an optional slot.
type Assignment struct {
	ID             string    `json:"id"`
	AssignableType string    `json:"assignable_type"`
	AssignableID   string    `json:"assignable_id"`
	ResourceType   string    `json:"resource_type"`
	ResourceID     string    `json:"resource_id"`
	Slot           string    `json:"slot"`
	CreatedBy      int64     `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// ResourceInfo is the enriched representation of the attached resource.
type ResourceInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// File fields
	Filename string `json:"filename,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	// Preset fields
	Code      string `json:"code,omitempty"`
	Label     string `json:"label,omitempty"`
	GroupSlug string `json:"group_slug,omitempty"`
	// Ingredient fields
	Metadata string `json:"metadata,omitempty"`
}

// AssignmentWithResource is the API view: the assignment plus resolved resource.
type AssignmentWithResource struct {
	Assignment
	Resource *ResourceInfo `json:"resource"`
}

type AssignRequest struct {
	ResourceType string `json:"resource_type" binding:"required"`
	ResourceID   string `json:"resource_id" binding:"required"`
	Slot         string `json:"slot"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists assignments in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const assignCols = `id, assignable_type, assignable_id, resource_type, resource_id,
	slot, COALESCE(created_by, 0) AS created_by, created_at`

func scanAssignment(a *Assignment, sc interface {
	Scan(dest ...interface{}) error
}) error {
	return sc.Scan(&a.ID, &a.AssignableType, &a.AssignableID, &a.ResourceType, &a.ResourceID,
		&a.Slot, &a.CreatedBy, &a.CreatedAt)
}

// Create inserts an assignment (idempotent via the unique constraint).
func (s *Store) Create(a *Assignment) error {
	query := `INSERT INTO assignments (id, assignable_type, assignable_id, resource_type, resource_id, slot, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (assignable_type, assignable_id, resource_type, resource_id, slot) DO UPDATE SET slot = EXCLUDED.slot
		RETURNING created_at`
	return s.db.QueryRow(query, a.ID, a.AssignableType, a.AssignableID, a.ResourceType,
		a.ResourceID, a.Slot, a.CreatedBy).Scan(&a.CreatedAt)
}

// ListByTarget returns all assignments of one element, enriched with the resource.
func (s *Store) ListByTarget(targetType, targetID string) ([]AssignmentWithResource, error) {
	rows, err := s.db.Query(`
		SELECT a.id, a.assignable_type, a.assignable_id, a.resource_type, a.resource_id,
		       a.slot, COALESCE(a.created_by,0), a.created_at,
		       COALESCE(f.filename,''), COALESCE(f.mime_type,''),
		       COALESCE(p.code,''), COALESCE(p.label,''), COALESCE(pg.slug,''),
		       COALESCE(i.name,''), COALESCE(i.metadata,'{}')
		FROM assignments a
		LEFT JOIN files f ON a.resource_type = 'file' AND f.id = a.resource_id
		LEFT JOIN presets p ON a.resource_type = 'preset' AND p.id = a.resource_id
		LEFT JOIN preset_groups pg ON p.group_id = pg.id
		LEFT JOIN ingredients i ON a.resource_type = 'ingredient' AND i.id = a.resource_id
		WHERE a.assignable_type = $1 AND a.assignable_id = $2
		ORDER BY a.created_at ASC`, targetType, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AssignmentWithResource
	for rows.Next() {
		var a AssignmentWithResource
		var filename, mimeType, code, label, groupSlug, ingName, metadata string
		if err := rows.Scan(&a.ID, &a.AssignableType, &a.AssignableID, &a.ResourceType, &a.ResourceID,
			&a.Slot, &a.CreatedBy, &a.CreatedAt,
			&filename, &mimeType, &code, &label, &groupSlug, &ingName, &metadata); err != nil {
			return nil, err
		}
		switch a.ResourceType {
		case ResourceFile:
			a.Resource = &ResourceInfo{ID: a.ResourceID, Name: filename, Kind: ResourceFile,
				Filename: filename, MimeType: mimeType}
		case ResourcePreset:
			a.Resource = &ResourceInfo{ID: a.ResourceID, Name: label, Kind: ResourcePreset,
				Code: code, Label: label, GroupSlug: groupSlug}
		case ResourceIngredient:
			a.Resource = &ResourceInfo{ID: a.ResourceID, Name: ingName, Kind: ResourceIngredient, Metadata: metadata}
		default:
			a.Resource = &ResourceInfo{ID: a.ResourceID, Kind: a.ResourceType}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListByResource returns assignments of one resource across all targets.
func (s *Store) ListByResource(resourceType, resourceID string) ([]AssignmentWithResource, error) {
	rows, err := s.db.Query(`SELECT `+assignCols+` FROM assignments
		WHERE resource_type = $1 AND resource_id = $2 ORDER BY created_at ASC`, resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AssignmentWithResource
	for rows.Next() {
		var a AssignmentWithResource
		if err := scanAssignment(&a.Assignment, rows); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Remove deletes one assignment by id.
func (s *Store) Remove(id string) error {
	result, err := s.db.Exec(`DELETE FROM assignments WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return errors.New("assignment not found")
	}
	return nil
}

// RemoveByTargetResource removes the assignment matching target+resource
// (the inverse lookup used by DELETE endpoints keyed by target).
func (s *Store) RemoveByTargetResource(targetType, targetID, resourceType, resourceID, slot string) error {
	var result sql.Result
	var err error
	if slot == "" {
		result, err = s.db.Exec(`DELETE FROM assignments
			WHERE assignable_type = $1 AND assignable_id = $2 AND resource_type = $3 AND resource_id = $4`,
			targetType, targetID, resourceType, resourceID)
	} else {
		result, err = s.db.Exec(`DELETE FROM assignments
			WHERE assignable_type = $1 AND assignable_id = $2 AND resource_type = $3 AND resource_id = $4 AND slot = $5`,
			targetType, targetID, resourceType, resourceID, slot)
	}
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return errors.New("assignment not found")
	}
	return nil
}

// NextFreeSlot returns the lowest "[ImageN]" slot not yet used by the target,
// so auto-assigned slots never collide (mirrors dcs-back's chapter slot logic).
func (s *Store) NextFreeSlot(targetType, targetID string) (string, error) {
	rows, err := s.db.Query(`SELECT slot FROM assignments
		WHERE assignable_type = $1 AND assignable_id = $2 AND slot LIKE '[Image%'`,
		targetType, targetID)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	used := map[int]bool{}
	for rows.Next() {
		var slot string
		if err := rows.Scan(&slot); err != nil {
			return "", err
		}
		if n := slotNum(slot); n > 0 {
			used[n] = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	n := 1
	for used[n] {
		n++
	}
	return fmt.Sprintf("[Image%d]", n), nil
}

// slotNum extracts the trailing number of a slot like "[Image5]" (→ 5).
func slotNum(slot string) int {
	n := 0
	mult := 1
	for i := len(slot) - 1; i >= 0 && slot[i] >= '0' && slot[i] <= '9'; i-- {
		n += int(slot[i]-'0') * mult
		mult *= 10
	}
	return n
}

// TargetExists validates the assignable id against its table.
func (s *Store) TargetExists(targetType, targetID string) (bool, error) {
	var table string
	switch targetType {
	case TargetEvent:
		table = "events"
	case TargetProgram:
		table = "programs"
	case TargetPiece:
		table = "pieces"
	default:
		return false, fmt.Errorf("invalid assignable type %q", targetType)
	}
	var exists bool
	err := s.db.QueryRow(fmt.Sprintf(
		`SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1 AND deleted_at IS NULL)`, table), targetID).Scan(&exists)
	return exists, err
}

// ─── Service ────────────────────────────────────────────────────

// Service implements assignment business logic.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

// Assign attaches a resource to a target element.
func (s *Service) Assign(targetType, targetID string, req *AssignRequest, userID int64, autoSlot bool) (*AssignmentWithResource, error) {
	if !ValidTarget(targetType) {
		return nil, fmt.Errorf("invalid assignable type %q (valid: event, program, piece)", targetType)
	}
	if !ValidResource(req.ResourceType) {
		return nil, fmt.Errorf("invalid resource type %q (valid: ingredient, file, preset, skill, model)", req.ResourceType)
	}
	exists, err := s.store.TargetExists(targetType, targetID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("target %s %q not found", targetType, targetID)
	}

	slot := req.Slot
	if slot == "" && autoSlot {
		// Auto-assign the next [ImageN] slot for ordered resources.
		next, err := s.store.NextFreeSlot(targetType, targetID)
		if err != nil {
			return nil, err
		}
		slot = next
	}

	a := &Assignment{
		ID: uuid.New().String(), AssignableType: targetType, AssignableID: targetID,
		ResourceType: req.ResourceType, ResourceID: req.ResourceID, Slot: slot, CreatedBy: userID,
	}
	if err := s.store.Create(a); err != nil {
		return nil, err
	}
	return &AssignmentWithResource{Assignment: *a}, nil
}

// ListByTarget returns enriched assignments for an element.
func (s *Service) ListByTarget(targetType, targetID string) ([]AssignmentWithResource, error) {
	if !ValidTarget(targetType) {
		return nil, fmt.Errorf("invalid assignable type %q", targetType)
	}
	items, err := s.store.ListByTarget(targetType, targetID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []AssignmentWithResource{}
	}
	return items, nil
}

// ListByResource returns assignments of one resource.
func (s *Service) ListByResource(resourceType, resourceID string) ([]AssignmentWithResource, error) {
	if !ValidResource(resourceType) {
		return nil, fmt.Errorf("invalid resource type %q", resourceType)
	}
	items, err := s.store.ListByResource(resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []AssignmentWithResource{}
	}
	return items, nil
}

// Remove deletes an assignment by id.
func (s *Service) Remove(id string) error { return s.store.Remove(id) }

// Unassign removes target+resource assignment.
func (s *Service) Unassign(targetType, targetID, resourceType, resourceID, slot string) error {
	if !ValidTarget(targetType) || !ValidResource(resourceType) {
		return fmt.Errorf("invalid target or resource type")
	}
	return s.store.RemoveByTargetResource(targetType, targetID, resourceType, resourceID, slot)
}

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes assignment endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// ListByTarget handles GET /assignments/:targetType/:targetId
func (h *Handler) ListByTarget(c *gin.Context) {
	items, err := h.svc.ListByTarget(c.Param("targetType"), c.Param("targetId"))
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, items)
}

// ListByResource handles GET /assignments/resource/:resourceType/:resourceId
func (h *Handler) ListByResource(c *gin.Context) {
	items, err := h.svc.ListByResource(c.Param("resourceType"), c.Param("resourceId"))
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, items)
}

// Assign handles POST /assignments/:targetType/:targetId
func (h *Handler) Assign(c *gin.Context) {
	var req AssignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	userID := utils.UserIDFromContext(c)
	autoSlot := c.Query("auto_slot") == "true"
	a, err := h.svc.Assign(c.Param("targetType"), c.Param("targetId"), &req, userID, autoSlot)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Created(c, a)
}

// Unassign handles DELETE /assignments/:targetType/:targetId/:resourceType/:resourceId
func (h *Handler) Unassign(c *gin.Context) {
	err := h.svc.Unassign(c.Param("targetType"), c.Param("targetId"),
		c.Param("resourceType"), c.Param("resourceId"), c.Query("slot"))
	if err != nil {
		if err.Error() == "assignment not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "assignment removed")
}

// Remove handles DELETE /assignments/by-id/:id
func (h *Handler) Remove(c *gin.Context) {
	if err := h.svc.Remove(c.Param("id")); err != nil {
		if err.Error() == "assignment not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "assignment removed")
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers assignment routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "assignments" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/assignments")
	g.Use(authMw, tenantMw)
	{
		g.GET("/:targetType/:targetId", m.priv("ListByTarget"))
		g.GET("/resource/:resourceType/:resourceId", m.priv("ListByResource"))
		g.POST("/:targetType/:targetId", m.priv("Assign"))
		g.DELETE("/:targetType/:targetId/:resourceType/:resourceId", m.priv("Unassign"))
		g.DELETE("/by-id/:id", m.priv("Remove"))
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
		case "ListByTarget":
			hdl.ListByTarget(c)
		case "ListByResource":
			hdl.ListByResource(c)
		case "Assign":
			hdl.Assign(c)
		case "Unassign":
			hdl.Unassign(c)
		case "Remove":
			hdl.Remove(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}
