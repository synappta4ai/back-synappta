package model

import (
	"github.com/gin-gonic/gin"

	"synapta/internal/utils"
)

// Module exposes the read-only model catalog.
type Module struct{}

func NewModule() *Module { return &Module{} }

func (m *Module) Name() string { return "models" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, _, _ gin.HandlerFunc) {
	g := rg.Group("/models")
	g.Use(authMw)
	{
		g.GET("", m.List)
		g.GET("/:modality", m.ListByModality)
	}
}

// List handles GET /api/v1/models?modality=video
func (m *Module) List(c *gin.Context) {
	modality := Modality(c.Query("modality"))
	if modality != "" && !IsValidModality(string(modality)) {
		utils.BadRequest(c, "invalid modality: "+string(modality))
		return
	}
	utils.Success(c, List(modality))
}

// ListByModality handles GET /api/v1/models/:modality
func (m *Module) ListByModality(c *gin.Context) {
	modality := Modality(c.Param("modality"))
	if !IsValidModality(string(modality)) {
		utils.NotFound(c, "unknown modality: "+string(modality))
		return
	}
	utils.Success(c, List(modality))
}
