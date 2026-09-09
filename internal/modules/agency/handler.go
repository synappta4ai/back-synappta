package agency

import (
	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// Handler exposes agency endpoints that are not modality-specific
// (logs, traces, generated assets, gallery sync, admin views).
type Handler struct {
	core *Core
}

// NewHandler builds the agency handler over a per-tenant core.
func NewHandler(core *Core) *Handler { return &Handler{core: core} }

// ─── Asset sync ─────────────────────────────────────────────────

// SyncAsset handles POST /agency/sync-asset
func (h *Handler) SyncAsset(c *gin.Context) {
	var req struct {
		Model  string `json:"model" binding:"required"`
		FileID string `json:"file_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.core.SyncAsset(req.Model, req.FileID)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// ListSyncedAssets handles GET /agency/synced-assets?model=...
func (h *Handler) ListSyncedAssets(c *gin.Context) {
	modelName := c.Query("model")
	if modelName == "" {
		utils.BadRequest(c, "model query parameter is required")
		return
	}
	assets, err := h.core.ListSyncedAssets(modelName)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if assets == nil {
		assets = []ModelAsset{}
	}
	utils.Success(c, assets)
}

// ─── Generation logs ────────────────────────────────────────────

// ListGenerationLogs handles GET /agency/logs/generation
func (h *Handler) ListGenerationLogs(c *gin.Context) {
	var f ListLogsFilter
	if err := c.ShouldBindQuery(&f); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 || f.Limit > 100 {
		f.Limit = 20
	}
	result, err := h.core.ListLogs(f)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// GetGenerationLogsCostSummary handles GET /agency/logs/generation/cost-summary
func (h *Handler) GetGenerationLogsCostSummary(c *gin.Context) {
	var f ListLogsFilter
	if err := c.ShouldBindQuery(&f); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	result, err := h.core.SumLogsCost(f)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// GetGenerationLog handles GET /agency/logs/generation/:id
func (h *Handler) GetGenerationLog(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		utils.BadRequest(c, "id is required")
		return
	}
	result, err := h.core.GetLog(id)
	if err != nil {
		if err.Error() == "generation log not found: "+id {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// ─── Server communications ──────────────────────────────────────

// ListServerCommunications handles GET /agency/logs/server-communications
func (h *Handler) ListServerCommunications(c *gin.Context) {
	page := atoiDefault(c.Query("page"), 1)
	limit := atoiDefault(c.Query("limit"), 20)
	result, err := h.core.ListComms(ServerCommFilter{
		TaskID:    c.Query("task_id"),
		ModelName: c.Query("model_name"),
		Page:      page,
		Limit:     limit,
	})
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// GetServerCommunication handles GET /agency/logs/server-communications/:id
func (h *Handler) GetServerCommunication(c *gin.Context) {
	result, err := h.core.GetComm(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if result == nil {
		utils.NotFound(c, "server communication not found")
		return
	}
	utils.Success(c, result)
}

// ─── Generated assets ───────────────────────────────────────────

// ListGeneratedAssets handles GET /agency/assets?piece_id=...
func (h *Handler) ListGeneratedAssets(c *gin.Context) {
	pieceID := c.Query("piece_id")
	if pieceID == "" {
		utils.BadRequest(c, "piece_id query parameter is required")
		return
	}
	assets, err := h.core.ListAssets(pieceID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, assets)
} // ─── Module ─────────────────────────────────────────────────────

// CoreResolver supplies the tenant-scoped core for each request.
type CoreResolver func(c *gin.Context) (*Core, bool)

// Module registers the agency routes. Modality sub-routers (video, image,
// audio, text) are attached by their own modules via AttachModality.
type Module struct {
	resolve    CoreResolver
	modalities map[string]func(rg *gin.RouterGroup)
}

// NewModule builds the agency module.
func NewModule(resolve CoreResolver) *Module {
	return &Module{resolve: resolve, modalities: map[string]func(rg *gin.RouterGroup){}}
}

// Name returns the module name.
func (m *Module) Name() string { return "agency" }

// AttachModality lets a modality module (video/image/audio/text) register its
// generate/status/cancel/preview endpoints under /agency/<modality>.
func (m *Module) AttachModality(name string, fn func(rg *gin.RouterGroup)) {
	m.modalities[name] = fn
}

// Register sets up the agency routes on the API v1 group.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, adminMw gin.HandlerFunc) {
	g := rg.Group("/agency")
	g.Use(authMw, tenantMw)
	{
		// Asset sync
		g.POST("/sync-asset", m.priv("SyncAsset"))
		g.GET("/synced-assets", m.priv("ListSyncedAssets"))

		// Logs
		g.GET("/logs/generation/cost-summary", m.priv("GetGenerationLogsCostSummary"))
		g.GET("/logs/generation", m.priv("ListGenerationLogs"))
		g.GET("/logs/generation/:id", m.priv("GetGenerationLog"))
		g.GET("/logs/server-communications", m.priv("ListServerCommunications"))
		g.GET("/logs/server-communications/:id", m.priv("GetServerCommunication"))

		// Generated assets
		g.GET("/assets", m.priv("ListGeneratedAssets"))

		// Modality routes (video/image/audio/text).
		for name, fn := range m.modalities {
			sub := g.Group("/" + name)
			fn(sub)
		}
	}
}

// priv resolves the tenant core then dispatches to the named handler method.
func (m *Module) priv(method string) gin.HandlerFunc {
	return func(c *gin.Context) {
		core, ok := m.resolve(c)
		if !ok {
			return
		}
		hdl := NewHandler(core)
		switch method {
		case "SyncAsset":
			hdl.SyncAsset(c)
		case "ListSyncedAssets":
			hdl.ListSyncedAssets(c)
		case "ListGenerationLogs":
			hdl.ListGenerationLogs(c)
		case "GetGenerationLogsCostSummary":
			hdl.GetGenerationLogsCostSummary(c)
		case "GetGenerationLog":
			hdl.GetGenerationLog(c)
		case "ListServerCommunications":
			hdl.ListServerCommunications(c)
		case "GetServerCommunication":
			hdl.GetServerCommunication(c)
		case "ListGeneratedAssets":
			hdl.ListGeneratedAssets(c)
		default:
			utils.InternalError(c, "unknown handler method: "+method)
		}
	}
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
