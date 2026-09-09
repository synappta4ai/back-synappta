// Package agencytext wires text generation (Claude) into the agency pipeline
// and exposes the /agency/text endpoints. The generator implementation
// lives in claude_text.go.
package agencytext

import (
	"strings"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/agency"
	"synapta/internal/utils"
)

// ─── HTTP layer ─────────────────────────────────────────────────

// CoreResolver supplies the tenant-scoped core for each request.
type CoreResolver func(c *gin.Context) (*agency.Core, bool)

// Service adapts text requests onto the per-tenant agency core.
type Service struct {
	resolve CoreResolver
}

// NewService builds the text service.
func NewService(resolve CoreResolver) *Service { return &Service{resolve: resolve} }

// Generate handles POST /agency/text/generate
func (s *Service) Generate(c *gin.Context) {
	var req agency.GenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.ResourceType = "text"

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

// GetStatus handles GET /agency/text/status/:taskId
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

// CancelTask handles DELETE /agency/text/task/:taskId
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

// PreviewPayload handles POST /agency/text/preview
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

// RegisterRoutes attaches the text endpoints to the /agency group.
func RegisterRoutes(rg *gin.RouterGroup, svc *Service) {
	rg.POST("/generate", svc.Generate)
	rg.GET("/status/:taskId", svc.GetStatus)
	rg.DELETE("/task/:taskId", svc.CancelTask)
	rg.POST("/preview", svc.PreviewPayload)
}
