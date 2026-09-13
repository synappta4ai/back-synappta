// Package agencyvideo wires video generation (Seedance, Seedance Gallery,
// Seedance 2.5) into the agency pipeline and exposes the /agency/video endpoints.
package agencyvideo

import (
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/agency"
	"synapta/internal/utils"
)

// ValidRatios are the aspect ratios supported by video generators.
var ValidRatios = map[string]bool{
	"16:9": true,
	"9:16": true,
	"1:1":  true,
	"4:3":  true,
	"3:4":  true,
	"21:9": true,
}

// ValidResolutionsVideo are the resolutions supported by video generators.
var ValidResolutionsVideo = map[string]bool{
	"480p":  true,
	"720p":  true,
	"1080p": true,
}

var videoURLPattern = regexp.MustCompile(`^https?://`)

// isFastModel reports whether the model id refers to the "fast" video variant.
func isFastModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "fast")
}

// safeShortID keeps only alphanumerics and whitespace for weak-id contexts.
func safeShortID(s string) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9 ]`)
	return re.ReplaceAllString(s, "")
}

func findVideoURL(result map[string]interface{}, depth int) string {
	if result == nil || depth > 4 {
		return ""
	}
	// Direct keys: "url" (generic) or "video_url" / "videoUrl" — BytePlus Ark
	// nests the download under content.video_url even at the top level.
	for _, key := range []string{"url", "video_url", "videoUrl"} {
		if u, ok := result[key].(string); ok && videoURLPattern.MatchString(u) {
			return u
		}
	}
	for _, v := range result {
		switch val := v.(type) {
		case map[string]interface{}:
			if u := findVideoURL(val, depth+1); u != "" {
				return u
			}
		case []interface{}:
			for _, item := range val {
				if m, ok := item.(map[string]interface{}); ok {
					if u := findVideoURL(m, depth+1); u != "" {
						return u
					}
				}
			}
		}
	}
	return ""
}

// ─── HTTP layer ─────────────────────────────────────────────────

// CoreResolver supplies the tenant-scoped core for each request.
type CoreResolver func(c *gin.Context) (*agency.Core, bool)

// Service adapts video requests onto the per-tenant agency core.
type Service struct {
	resolve CoreResolver
}

// NewService builds the video service.
func NewService(resolve CoreResolver) *Service { return &Service{resolve: resolve} }

// Generate handles POST /agency/video/generate
func (s *Service) Generate(c *gin.Context) {
	var req agency.GenerateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.ResourceType = "video"
	agency.AttachCaller(&req, c)

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

// GetStatus handles GET /agency/video/status/:taskId
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

// CancelTask handles DELETE /agency/video/task/:taskId
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

// PreviewPayload handles POST /agency/video/preview
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

// RegisterRoutes attaches the video endpoints to the /agency group.
func RegisterRoutes(rg *gin.RouterGroup, svc *Service) {
	rg.POST("/generate", svc.Generate)
	rg.GET("/status/:taskId", svc.GetStatus)
	rg.DELETE("/task/:taskId", svc.CancelTask)
	rg.POST("/preview", svc.PreviewPayload)
}
