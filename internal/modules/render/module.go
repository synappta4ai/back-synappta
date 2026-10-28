// Package render powers the video editor: it stores editing projects (a JSON
// timeline) and renders them to MP4 on the server with ffmpeg. The browser
// only edits data and plays the sources; the heavy work never runs on the
// user's machine.
//
// Isolation rules (this module must never affect the rest of the API):
//   - routes live under /editor and /render only;
//   - every goroutine recovers, so a bug fails one job, never the process;
//   - ffmpeg concurrency is capped by its own semaphore (RENDER_MAX_CONCURRENT)
//     and never shares the infoproduct one.
package render

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/file"
	"synapta/internal/utils"
)

const (
	maxBodyBytes     = 2 << 20
	maxActivePerUser = 3
)

// FilesResolver and PoolResolver supply the tenant-scoped services per request.
type (
	FilesResolver func(c *gin.Context) (*file.Handler, bool)
	PoolResolver  func(c *gin.Context) (*sql.DB, bool)
)

// Module registers the /editor and /render routes.
type Module struct {
	files      FilesResolver
	pool       PoolResolver
	outputsDir string

	sem        chan struct{}
	maxSeconds float64
	timeout    time.Duration
	threads    int

	ctx    context.Context
	cancel context.CancelFunc
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v > 0 {
		return v
	}
	return def
}

// NewModule builds the module. Tunables (all optional):
// RENDER_MAX_CONCURRENT (1), RENDER_MAX_SECONDS (600), RENDER_TIMEOUT_MINUTES
// (20), RENDER_THREADS (half of the CPUs).
func NewModule(files FilesResolver, pool PoolResolver, outputsDir string) *Module {
	ctx, cancel := context.WithCancel(context.Background())
	threads := runtime.NumCPU() / 2
	if threads < 1 {
		threads = 1
	}
	return &Module{
		files: files, pool: pool, outputsDir: outputsDir,
		sem:        make(chan struct{}, envInt("RENDER_MAX_CONCURRENT", 1)),
		maxSeconds: float64(envInt("RENDER_MAX_SECONDS", 600)),
		timeout:    time.Duration(envInt("RENDER_TIMEOUT_MINUTES", 20)) * time.Minute,
		threads:    envInt("RENDER_THREADS", threads),
		ctx:        ctx, cancel: cancel,
	}
}

// Shutdown cancels running renders (called on server stop).
func (m *Module) Shutdown() { m.cancel() }

// Name implements modules.Module.
func (m *Module) Name() string { return "render" }

// Register implements modules.Module.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	ed := rg.Group("/editor")
	ed.Use(authMw, tenantMw)
	{
		ed.GET("/projects", m.ListProjects)
		ed.POST("/projects", m.CreateProject)
		ed.GET("/projects/:id", m.GetProject)
		ed.PUT("/projects/:id", m.UpdateProject)
		ed.DELETE("/projects/:id", m.DeleteProject)
	}
	r := rg.Group("/render")
	r.Use(authMw, tenantMw)
	{
		r.GET("/jobs", m.ListJobs)
		r.POST("/jobs", m.CreateJob)
		r.GET("/jobs/:id", m.GetJob)
		r.GET("/media", m.Media)
		r.GET("/poster", m.Poster)
		r.GET("/jobs/:id/download", m.Download)
	}
}

func (m *Module) store(c *gin.Context) (*store, bool) {
	db, ok := m.pool(c)
	if !ok || db == nil {
		utils.InternalError(c, "tenant database unavailable")
		return nil, false
	}
	return &store{db: db}, true
}

// readJSON reads a size-capped JSON body.
func readJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		utils.Error(c, http.StatusRequestEntityTooLarge, "el proyecto es demasiado grande")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		utils.BadRequest(c, "JSON invalido: "+err.Error())
		return false
	}
	return true
}

// isJSONObject reports whether raw is a JSON object (drafts may be incomplete,
// so we only require the document to be well-formed).
func isJSONObject(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return strings.HasPrefix(t, "{") && json.Valid(raw)
}

// ─── Projects ───────────────────────────────────────────────────

type projectBody struct {
	Name    *string         `json:"name"`
	Project json.RawMessage `json:"project"`
}

// ListProjects handles GET /editor/projects.
func (m *Module) ListProjects(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	list, err := st.listProjects(utils.UserIDFromContext(c))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, list)
}

// CreateProject handles POST /editor/projects.
func (m *Module) CreateProject(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	var b projectBody
	if !readJSON(c, &b) {
		return
	}
	if len(b.Project) == 0 {
		b.Project = json.RawMessage(`{}`)
	}
	if !isJSONObject(b.Project) {
		utils.BadRequest(c, "project debe ser un objeto JSON")
		return
	}
	name := "Sin titulo"
	if b.Name != nil && strings.TrimSpace(*b.Name) != "" {
		name = strings.TrimSpace(*b.Name)
	}
	p, err := st.createProject(utils.UserIDFromContext(c), name, b.Project)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Created(c, p)
}

// GetProject handles GET /editor/projects/:id.
func (m *Module) GetProject(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	p, err := st.getProject(utils.UserIDFromContext(c), c.Param("id"))
	if err != nil {
		utils.NotFound(c, "proyecto no encontrado")
		return
	}
	if p == nil {
		utils.NotFound(c, "proyecto no encontrado")
		return
	}
	utils.Success(c, p)
}

// UpdateProject handles PUT /editor/projects/:id.
func (m *Module) UpdateProject(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	var b projectBody
	if !readJSON(c, &b) {
		return
	}
	if len(b.Project) > 0 && !isJSONObject(b.Project) {
		utils.BadRequest(c, "project debe ser un objeto JSON")
		return
	}
	if b.Name != nil {
		n := strings.TrimSpace(*b.Name)
		if n == "" {
			n = "Sin titulo"
		}
		b.Name = &n
	}
	found, err := st.updateProject(utils.UserIDFromContext(c), c.Param("id"), b.Name, b.Project)
	if err != nil {
		utils.NotFound(c, "proyecto no encontrado")
		return
	}
	if !found {
		utils.NotFound(c, "proyecto no encontrado")
		return
	}
	utils.Message(c, "proyecto actualizado")
}

// DeleteProject handles DELETE /editor/projects/:id.
func (m *Module) DeleteProject(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	found, err := st.deleteProject(utils.UserIDFromContext(c), c.Param("id"))
	if err != nil || !found {
		utils.NotFound(c, "proyecto no encontrado")
		return
	}
	utils.Message(c, "proyecto eliminado")
}

// ─── Jobs ───────────────────────────────────────────────────────

type jobBody struct {
	ProjectID string          `json:"project_id"`
	Project   json.RawMessage `json:"project"`
	// Format: mp4 (default) | mp4_h265 | mov | webm | gif.
	Format string `json:"format"`
	// SaveToLibrary keeps a copy in the tenant library. The editor sends false:
	// the file is only kept until the user downloads it to their machine.
	SaveToLibrary *bool `json:"save_to_library"`
}

// CreateJob handles POST /render/jobs. It validates the project, queues the
// job and answers 202 immediately; the render runs in the background.
func (m *Module) CreateJob(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	files, ok := m.files(c)
	if !ok {
		return
	}
	userID := utils.UserIDFromContext(c)
	var b jobBody
	if !readJSON(c, &b) {
		return
	}
	var projectID *string
	doc := b.Project
	if id := strings.TrimSpace(b.ProjectID); id != "" {
		row, err := st.getProject(userID, id)
		if err != nil || row == nil {
			utils.NotFound(c, "proyecto no encontrado")
			return
		}
		projectID = &id
		if len(doc) == 0 {
			doc = row.Project
		}
	}
	if len(doc) == 0 {
		utils.BadRequest(c, "envia project_id o project")
		return
	}
	var p Project
	if err := json.Unmarshal(doc, &p); err != nil {
		utils.BadRequest(c, "proyecto invalido: "+err.Error())
		return
	}
	if err := p.normalize(); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if n, err := st.countActive(userID); err == nil && n >= maxActivePerUser {
		utils.Error(c, http.StatusTooManyRequests, "ya tienes renders en curso; espera a que terminen")
		return
	}
	format, err := normalizeFormat(b.Format)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	saveToLibrary := true
	if b.SaveToLibrary != nil {
		saveToLibrary = *b.SaveToLibrary
	}
	snapshot, _ := json.Marshal(&p)
	job, err := st.createJob(userID, projectID, snapshot, format)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	go m.runJob(st, files, job.ID, &p, format, saveToLibrary)
	c.JSON(http.StatusAccepted, gin.H{"data": job, "success": true, "message": "queued"})
}

// GetJob handles GET /render/jobs/:id.
func (m *Module) GetJob(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	j, err := st.getJob(utils.UserIDFromContext(c), c.Param("id"))
	if err != nil || j == nil {
		utils.NotFound(c, "job no encontrado")
		return
	}
	utils.Success(c, j)
}

// Download streams a download-only render to the user's machine and removes
// the temporary file right after it was sent: nothing stays on the server.
func (m *Module) Download(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	path, format, found, err := st.tempOutput(utils.UserIDFromContext(c), c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if !found {
		utils.NotFound(c, "la descarga expiro o el video se guardo en la biblioteca")
		return
	}
	if _, statErr := os.Stat(path); statErr != nil {
		st.clearTempOutput(c.Param("id"))
		utils.NotFound(c, "la descarga expiro")
		return
	}
	spec := formatOf(format)
	name := "video-" + time.Now().Format("2006-01-02-1504") + "." + spec.Ext
	c.Header("Content-Type", spec.MimeType)
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.File(path)
	_ = os.Remove(path)
	_ = os.RemoveAll(filepath.Dir(path))
	st.clearTempOutput(c.Param("id"))
}

// ListJobs handles GET /render/jobs.
func (m *Module) ListJobs(c *gin.Context) {
	st, ok := m.store(c)
	if !ok {
		return
	}
	list, err := st.listJobs(utils.UserIDFromContext(c))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, list)
}
