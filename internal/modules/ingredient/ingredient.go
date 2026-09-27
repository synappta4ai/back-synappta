// Package ingredient replaces dcs-back's "character": reusable generation
// assets attached to events, programs or pieces (talent, props, brands).
// Uses the agnostic assignments table for target attachment.
package ingredient

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/utils"
)

// Valid ingredient kinds.
const (
	TypeCharacter = "character"
	TypeLocation  = "location"
	TypeProp      = "prop"
)

// validTypes is the whitelist enforced on create, update and list filters.
var validTypes = map[string]bool{
	TypeCharacter: true, TypeLocation: true, TypeProp: true,
}

// Ingredient is a reusable generation asset.
type Ingredient struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Metadata    string     `json:"metadata"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

// SyncModelItem reports which AI models a file is already synced with.
type SyncModelItem struct {
	ModelID string `json:"model_id"`
	Name    string `json:"name"`
}

// IngredientFile is a file attached to an ingredient.
type IngredientFile struct {
	FileID       string          `json:"file_id"`
	Role         string          `json:"role"`
	Filename     string          `json:"filename"`
	URL          string          `json:"url"`
	ThumbnailURL string          `json:"thumbnail_url"`
	MimeType     string          `json:"mime_type"`
	Category     string          `json:"category"`
	Format       string          `json:"format"`
	Size         int64           `json:"size"`
	CreatedAt    time.Time       `json:"created_at"`
	SyncedModels []SyncModelItem `json:"synced_models,omitempty"`
}

// IngredientWithFiles is the enriched API representation.
type IngredientWithFiles struct {
	Ingredient Ingredient       `json:"ingredient"`
	Files      []IngredientFile `json:"files"`
}

type CreateIngredientRequest struct {
	Type        string `json:"type"`
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	Metadata    string `json:"metadata"`
}

type UpdateIngredientRequest struct {
	Type        *string `json:"type"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Metadata    *string `json:"metadata"`
}

type AddFileRequest struct {
	FileID string `json:"file_id" binding:"required"`
	Role   string `json:"role"`
}

type PaginatedIngredients struct {
	Items      []IngredientWithFiles `json:"items"`
	Total      int                   `json:"total"`
	Page       int                   `json:"page"`
	PageSize   int                   `json:"pageSize"`
	TotalPages int                   `json:"totalPages"`
}

// ─── Store ──────────────────────────────────────────────────────

// Store persists ingredients in the tenant schema.
type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Create(ing *Ingredient) error {
	query := `INSERT INTO ingredients (id, type, name, description, metadata)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, ing.ID, ing.Type, ing.Name, ing.Description, ing.Metadata).
		Scan(&ing.CreatedAt, &ing.UpdatedAt)
}

// FindActiveByTypeAndName returns the newest non-deleted ingredient with the
// given type+name, or nil when there is no match. Used to dedup ingredient
// creation (same type + same name = same ingredient, no copies).
func (s *Store) FindActiveByTypeAndName(ingType, name string) (*Ingredient, error) {
	ing := &Ingredient{}
	err := s.db.QueryRow(`SELECT id, type, name, description, metadata, created_at, updated_at, deleted_at
		FROM ingredients WHERE type = $1 AND name = $2 AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, ingType, name).
		Scan(&ing.ID, &ing.Type, &ing.Name, &ing.Description, &ing.Metadata, &ing.CreatedAt, &ing.UpdatedAt, &ing.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ing, nil
}

func (s *Store) GetByID(id string) (*Ingredient, error) {
	ing := &Ingredient{}
	err := s.db.QueryRow(`SELECT id, type, name, description, metadata, created_at, updated_at, deleted_at
		FROM ingredients WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&ing.ID, &ing.Type, &ing.Name, &ing.Description, &ing.Metadata, &ing.CreatedAt, &ing.UpdatedAt, &ing.DeletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return ing, err
}

func (s *Store) List() ([]Ingredient, error) {
	rows, err := s.db.Query(`SELECT id, type, name, description, metadata, created_at, updated_at, deleted_at
		FROM ingredients WHERE deleted_at IS NULL ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Ingredient
	for rows.Next() {
		var ing Ingredient
		if err := rows.Scan(&ing.ID, &ing.Type, &ing.Name, &ing.Description, &ing.Metadata,
			&ing.CreatedAt, &ing.UpdatedAt, &ing.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, ing)
	}
	return out, rows.Err()
}

func (s *Store) ListPage(page, pageSize int, search, ingType string) ([]Ingredient, int, error) {
	where := "WHERE deleted_at IS NULL"
	args := []interface{}{}
	argIdx := 1
	if ingType != "" {
		where += fmt.Sprintf(" AND type = $%d", argIdx)
		args = append(args, ingType)
		argIdx++
	}
	if search != "" {
		where += fmt.Sprintf(" AND (name ILIKE $%d OR description ILIKE $%d)", argIdx, argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM ingredients "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	query := `SELECT id, type, name, description, metadata, created_at, updated_at, deleted_at
		FROM ingredients ` + where + ` ORDER BY created_at DESC
		LIMIT ` + itoa(pageSize) + ` OFFSET ` + itoa(offset)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Ingredient
	for rows.Next() {
		var ing Ingredient
		if err := rows.Scan(&ing.ID, &ing.Type, &ing.Name, &ing.Description, &ing.Metadata,
			&ing.CreatedAt, &ing.UpdatedAt, &ing.DeletedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, ing)
	}
	return out, total, rows.Err()
}

func (s *Store) Update(id string, updates map[string]interface{}) error {
	if len(updates) == 0 {
		return nil
	}
	query := "UPDATE ingredients SET updated_at = NOW()"
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
		return fmt.Errorf("ingredient not found")
	}
	return nil
}

func (s *Store) SoftDelete(id string) error {
	result, err := s.db.Exec(`UPDATE ingredients SET deleted_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("ingredient not found")
	}
	return nil
}

// ─── Ingredient-File relations ─────────────────────────────────

func (s *Store) AddFile(ingredientID, fileID, role string) error {
	if role == "" {
		role = "reference"
	}
	_, err := s.db.Exec(`INSERT INTO ingredient_files (ingredient_id, file_id, role)
		VALUES ($1, $2, $3) ON CONFLICT (ingredient_id, file_id, role) DO NOTHING`,
		ingredientID, fileID, role)
	return err
}

func (s *Store) RemoveFile(ingredientID, fileID string) error {
	_, err := s.db.Exec(`DELETE FROM ingredient_files WHERE ingredient_id = $1 AND file_id = $2`,
		ingredientID, fileID)
	return err
}

func (s *Store) ListFiles(ingredientID string) ([]IngredientFile, error) {
	rows, err := s.db.Query(`
		SELECT ifile.file_id, ifile.role, ifile.created_at,
		       COALESCE(f.filename, ''), COALESCE(f.size, 0), COALESCE(f.mime_type, ''),
		       COALESCE(f.category, ''), COALESCE(f.format, '')
		FROM ingredient_files ifile
		LEFT JOIN files f ON f.id = ifile.file_id
		WHERE ifile.ingredient_id = $1
		ORDER BY ifile.created_at ASC`, ingredientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []IngredientFile
	for rows.Next() {
		var f IngredientFile
		if err := rows.Scan(&f.FileID, &f.Role, &f.CreatedAt,
			&f.Filename, &f.Size, &f.MimeType, &f.Category, &f.Format); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) ListFilesForIngredients(ids []string) (map[string][]IngredientFile, error) {
	result := make(map[string][]IngredientFile)
	if len(ids) == 0 {
		return result, nil
	}
	args := make([]interface{}, len(ids))
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf(`
		SELECT ifile.ingredient_id, ifile.file_id, ifile.role, ifile.created_at,
		       COALESCE(f.filename, ''), COALESCE(f.size, 0), COALESCE(f.mime_type, ''),
		       COALESCE(f.category, ''), COALESCE(f.format, '')
		FROM ingredient_files ifile
		LEFT JOIN files f ON f.id = ifile.file_id
		WHERE ifile.ingredient_id IN (%s)
		ORDER BY ifile.ingredient_id, ifile.created_at ASC`, strings.Join(placeholders, ","))

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var ingredientID string
		var f IngredientFile
		if err := rows.Scan(&ingredientID, &f.FileID, &f.Role, &f.CreatedAt,
			&f.Filename, &f.Size, &f.MimeType, &f.Category, &f.Format); err != nil {
			return nil, err
		}
		result[ingredientID] = append(result[ingredientID], f)
	}
	return result, rows.Err()
}

// FindIngredientsByFileID returns ingredient ids that reference a file.
func (s *Store) FindIngredientsByFileID(fileID string) ([]string, error) {
	rows, err := s.db.Query("SELECT DISTINCT ingredient_id FROM ingredient_files WHERE file_id = $1", fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ─── Service ────────────────────────────────────────────────────

// FileEnricher enriches files with extra data (e.g. asset-sync info).
type FileEnricher func(files []IngredientFile)

// Service implements ingredient business logic.
type Service struct {
	store    *Store
	baseURL  string
	enricher FileEnricher
}

func NewService(store *Store, baseURL string) *Service {
	return &Service{store: store, baseURL: baseURL}
}

// SetFileEnricher attaches the asset-sync enrichment callback (wired in main).
func (s *Service) SetFileEnricher(fn FileEnricher) { s.enricher = fn }

func (s *Service) enrichFileURLs(files []IngredientFile) {
	for i := range files {
		files[i].URL = s.baseURL + "/api/v1/files/" + files[i].FileID + "/serve"
		files[i].ThumbnailURL = s.baseURL + "/api/v1/files/" + files[i].FileID + "/thumbnail"
	}
	if s.enricher != nil {
		s.enricher(files)
	}
}

func (s *Service) Create(req CreateIngredientRequest) (*Ingredient, error) {
	t := req.Type
	if t == "" {
		t = TypeCharacter // default kind
	}
	if !validTypes[t] {
		return nil, fmt.Errorf("invalid type %q: must be one of character, location, prop", t)
	}
	// Dedup: same type + same name = same ingredient. Reuse the existing row
	// instead of creating a copy (uploads retried by the UI hit this path).
	existing, err := s.store.FindActiveByTypeAndName(t, req.Name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	ing := &Ingredient{
		ID:          uuid.New().String(),
		Type:        t,
		Name:        req.Name,
		Description: req.Description,
		Metadata:    req.Metadata,
	}
	if ing.Metadata == "" {
		ing.Metadata = "{}"
	}
	if err := s.store.Create(ing); err != nil {
		return nil, err
	}
	return ing, nil
}

func (s *Service) GetByID(id string) (*Ingredient, error) { return s.store.GetByID(id) }

func (s *Service) GetByIDWithFiles(id string) (*IngredientWithFiles, error) {
	ing, err := s.store.GetByID(id)
	if err != nil {
		return nil, err
	}
	if ing == nil {
		return nil, nil
	}
	files, err := s.store.ListFiles(id)
	if err != nil {
		return nil, err
	}
	if files == nil {
		files = []IngredientFile{}
	}
	s.enrichFileURLs(files)
	return &IngredientWithFiles{Ingredient: *ing, Files: files}, nil
}

func (s *Service) ListPage(page, pageSize int, search, ingType string) (*PaginatedIngredients, error) {
	if ingType != "" && !validTypes[ingType] {
		return nil, fmt.Errorf("invalid type %q: must be one of character, location, prop", ingType)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	ings, total, err := s.store.ListPage(page, pageSize, search, ingType)
	if err != nil {
		return nil, err
	}
	if len(ings) == 0 {
		return &PaginatedIngredients{Items: []IngredientWithFiles{}, Total: 0,
			Page: page, PageSize: pageSize, TotalPages: 0}, nil
	}
	ids := make([]string, len(ings))
	for i, ing := range ings {
		ids[i] = ing.ID
	}
	filesMap, err := s.store.ListFilesForIngredients(ids)
	if err != nil {
		return nil, err
	}
	items := make([]IngredientWithFiles, len(ings))
	for i, ing := range ings {
		files := filesMap[ing.ID]
		if files == nil {
			files = []IngredientFile{}
		}
		s.enrichFileURLs(files)
		items[i] = IngredientWithFiles{Ingredient: ing, Files: files}
	}
	totalPages := (total + pageSize - 1) / pageSize
	return &PaginatedIngredients{Items: items, Total: total, Page: page,
		PageSize: pageSize, TotalPages: totalPages}, nil
}

func (s *Service) List() ([]IngredientWithFiles, error) {
	ings, err := s.store.List()
	if err != nil {
		return nil, err
	}
	if len(ings) == 0 {
		return []IngredientWithFiles{}, nil
	}
	ids := make([]string, len(ings))
	for i, ing := range ings {
		ids[i] = ing.ID
	}
	filesMap, err := s.store.ListFilesForIngredients(ids)
	if err != nil {
		return nil, err
	}
	result := make([]IngredientWithFiles, len(ings))
	for i, ing := range ings {
		files := filesMap[ing.ID]
		if files == nil {
			files = []IngredientFile{}
		}
		s.enrichFileURLs(files)
		result[i] = IngredientWithFiles{Ingredient: ing, Files: files}
	}
	return result, nil
}

func (s *Service) Update(id string, req UpdateIngredientRequest) (*Ingredient, error) {
	updates := make(map[string]interface{})
	if req.Type != nil {
		if !validTypes[*req.Type] {
			return nil, fmt.Errorf("invalid type %q: must be one of character, location, prop", *req.Type)
		}
		updates["type"] = *req.Type
	}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Metadata != nil {
		updates["metadata"] = *req.Metadata
	}
	if len(updates) == 0 {
		return s.store.GetByID(id)
	}
	if err := s.store.Update(id, updates); err != nil {
		return nil, err
	}
	return s.store.GetByID(id)
}

func (s *Service) SoftDelete(id string) error { return s.store.SoftDelete(id) }

func (s *Service) AddFile(ingredientID, fileID, role string) error {
	ing, err := s.store.GetByID(ingredientID)
	if err != nil {
		return err
	}
	if ing == nil {
		return fmt.Errorf("ingredient not found")
	}
	return s.store.AddFile(ingredientID, fileID, role)
}

func (s *Service) RemoveFile(ingredientID, fileID string) error {
	return s.store.RemoveFile(ingredientID, fileID)
}

func (s *Service) ListFiles(ingredientID string) ([]IngredientFile, error) {
	files, err := s.store.ListFiles(ingredientID)
	if err != nil {
		return nil, err
	}
	if files == nil {
		files = []IngredientFile{}
	}
	s.enrichFileURLs(files)
	return files, nil
}

// FindIngredientsByFileID returns ingredient ids referencing a file.
func (s *Service) FindIngredientsByFileID(fileID string) ([]string, error) {
	return s.store.FindIngredientsByFileID(fileID)
}

// ─── HTTP ───────────────────────────────────────────────────────

// Handler exposes ingredient endpoints.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Create handles POST /ingredients
func (h *Handler) Create(c *gin.Context) {
	var req CreateIngredientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ing, err := h.svc.Create(req)
	if err != nil {
		if strings.HasPrefix(err.Error(), "invalid type") {
			utils.BadRequest(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, ing)
}

// GetByID handles GET /ingredients/:id
func (h *Handler) GetByID(c *gin.Context) {
	ing, err := h.svc.GetByIDWithFiles(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if ing == nil {
		utils.NotFound(c, "ingredient not found")
		return
	}
	utils.Success(c, ing)
}

// ListPage handles GET /ingredients/page?type=character&q=...
func (h *Handler) ListPage(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	pageSize := atoiDefault(c.Query("pageSize"), 50)
	result, err := h.svc.ListPage(page, pageSize, c.Query("q"), c.Query("type"))
	if err != nil {
		if strings.HasPrefix(err.Error(), "invalid type") {
			utils.BadRequest(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// List handles GET /ingredients
func (h *Handler) List(c *gin.Context) {
	ings, err := h.svc.List()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if ings == nil {
		ings = []IngredientWithFiles{}
	}
	utils.Success(c, ings)
}

// Update handles PATCH /ingredients/:id
func (h *Handler) Update(c *gin.Context) {
	var req UpdateIngredientRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	ing, err := h.svc.Update(c.Param("id"), req)
	if err != nil {
		if err.Error() == "ingredient not found" {
			utils.NotFound(c, err.Error())
			return
		}
		if strings.HasPrefix(err.Error(), "invalid type") {
			utils.BadRequest(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, ing)
}

// SoftDelete handles DELETE /ingredients/:id
func (h *Handler) SoftDelete(c *gin.Context) {
	if err := h.svc.SoftDelete(c.Param("id")); err != nil {
		if err.Error() == "ingredient not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "ingredient deleted")
}

// AddFile handles POST /ingredients/:id/files
func (h *Handler) AddFile(c *gin.Context) {
	var req AddFileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if err := h.svc.AddFile(c.Param("id"), req.FileID, req.Role); err != nil {
		if err.Error() == "ingredient not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "file added to ingredient")
}

// RemoveFile handles DELETE /ingredients/:id/files/:fileId
func (h *Handler) RemoveFile(c *gin.Context) {
	if err := h.svc.RemoveFile(c.Param("id"), c.Param("fileId")); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "file removed from ingredient")
}

// ListFiles handles GET /ingredients/:id/files
func (h *Handler) ListFiles(c *gin.Context) {
	files, err := h.svc.ListFiles(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if files == nil {
		files = []IngredientFile{}
	}
	utils.Success(c, files)
}

// ─── Module ─────────────────────────────────────────────────────

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// Module registers ingredient routes.
type Module struct {
	resolve HandlerResolver
}

func NewModule(resolve HandlerResolver) *Module { return &Module{resolve: resolve} }

func (m *Module) Name() string { return "ingredients" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/ingredients")
	g.Use(authMw, tenantMw)
	{
		g.POST("", m.priv("Create"))
		g.GET("/page", m.priv("ListPage"))
		g.GET("", m.priv("List"))
		g.GET("/:id", m.priv("GetByID"))
		g.PATCH("/:id", m.priv("Update"))
		g.DELETE("/:id", m.priv("SoftDelete"))
		g.POST("/:id/files", m.priv("AddFile"))
		g.DELETE("/:id/files/:fileId", m.priv("RemoveFile"))
		g.GET("/:id/files", m.priv("ListFiles"))
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
		case "ListPage":
			hdl.ListPage(c)
		case "List":
			hdl.List(c)
		case "GetByID":
			hdl.GetByID(c)
		case "Update":
			hdl.Update(c)
		case "SoftDelete":
			hdl.SoftDelete(c)
		case "AddFile":
			hdl.AddFile(c)
		case "RemoveFile":
			hdl.RemoveFile(c)
		case "ListFiles":
			hdl.ListFiles(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}

// ─── Helpers ────────────────────────────────────────────────────

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	ok := true
	for _, r := range s {
		if r < '0' || r > '9' {
			ok = false
			break
		}
		n = n*10 + int(r-'0')
	}
	if !ok || n == 0 {
		return def
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
