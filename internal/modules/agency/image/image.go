// Package agencyimage wires image generation (Seedream, Gemini Nano/Pro)
// into the agency pipeline and exposes the /agency/image endpoints.
// Generator implementations live in model-named files: seedream.go,
// gemini_nano.go, gemini_nano_pro.go.
package agencyimage

import (
	"strings"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/agency"
	"synapta/internal/utils"
)

// ValidResolutionsImage lists supported image resolutions.
var ValidResolutionsImage = map[string]bool{
	"480p": true, "720p": true, "1080p": true, "1K": true, "2K": true, "4K": true,
}

// ─── HTTP layer ─────────────────────────────────────────────────

// CoreResolver supplies the tenant-scoped core for each request.
type CoreResolver func(c *gin.Context) (*agency.Core, bool)

// Service adapts image requests onto the per-tenant agency core.
type Service struct {
	resolve CoreResolver
}

// NewService builds the image service.
func NewService(resolve CoreResolver) *Service { return &Service{resolve: resolve} }

// Generate handles POST /agency/image/generate
func (s *Service) Generate(c *gin.Context) {
	var req agency.GenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.ResourceType = "image"

	core, ok := s.resolve(c)
	if !ok {
		return
	}
	result, err := core.GenerateUnified(&req)
	if err != nil {
		if strings.Contains(err.Error(), "model not found") {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Created(c, result)
}

// GetStatus handles GET /agency/image/status/:taskId
func (s *Service) GetStatus(c *gin.Context) {
	taskID := c.Param("taskId")
	if taskID == "" {
		utils.BadRequest(c, "taskId is required")
		return
	}
	core, ok := s.resolve(c)
	if !ok {
		return
	}
	result, err := core.GetStatusUnified(taskID)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// CancelTask handles DELETE /agency/image/task/:taskId
func (s *Service) CancelTask(c *gin.Context) {
	taskID := c.Param("taskId")
	if taskID == "" {
		utils.BadRequest(c, "taskId is required")
		return
	}
	core, ok := s.resolve(c)
	if !ok {
		return
	}
	if err := core.CancelTask(taskID); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "task cancelled")
}

// PreviewPayload handles POST /agency/image/preview
func (s *Service) PreviewPayload(c *gin.Context) {
	var req agency.GenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	core, ok := s.resolve(c)
	if !ok {
		return
	}
	result, err := core.PreviewPayload(&req)
	if err != nil {
		if strings.Contains(err.Error(), "model not found") {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// RegisterRoutes attaches the image endpoints to the /agency group.
func RegisterRoutes(rg *gin.RouterGroup, svc *Service) {
	rg.POST("/generate", svc.Generate)
	rg.GET("/status/:taskId", svc.GetStatus)
	rg.DELETE("/task/:taskId", svc.CancelTask)
	rg.POST("/preview", svc.PreviewPayload)
}
