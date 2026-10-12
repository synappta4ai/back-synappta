// Package mcp expone el backend de Synapta como servidor MCP remoto
// (Model Context Protocol) para agentes externos —Claude, ChatGPT, Codex,
// etc.—: descubren herramientas del dominio (proyectos, modelos,
// generaciones) y las ejecutan contra el tenant del token JWT.
//
// Se apoya en el SDK oficial (modelcontextprotocol/go-sdk) y en el mismo
// middleware de auth+tenant que el resto de la API: el cliente MCP envía el
// Bearer del login normal, así que no hay credenciales nuevas ni rutas
// públicas. El transporte es Streamable HTTP en modo stateless (SEP-2567):
// cada POST crea una sesión temporal, lo que permite resolver los servicios
// del tenant por request igual que cualquier otro módulo.
package mcp

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	mcpapi "github.com/modelcontextprotocol/go-sdk/mcp"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/event"
	"synapta/internal/toolkit"
	"synapta/internal/utils"
)

// CoreResolver supplies the tenant-scoped agency core per request.
type CoreResolver func(c *gin.Context) (*agency.Core, bool)

// EventsResolver supplies the tenant-scoped events handler per request.
type EventsResolver func(c *gin.Context) (*event.Handler, bool)

// Deps (servicios del tenant + identidad del usuario) y su inyección en el
// ctx viven en el toolkit compartido: el loop del asistente interno usa el
// mismo mecanismo.
type Deps = toolkit.Deps

// DepsFrom extrae los servicios del tenant del ctx (alias del toolkit).
func DepsFrom(ctx context.Context) (*Deps, bool) {
	return toolkit.DepsFrom(ctx)
}

// Module registra POST/GET/DELETE /api/v1/mcp sobre el grupo v1.
type Module struct {
	events  EventsResolver
	core    CoreResolver
	handler *mcpapi.StreamableHTTPHandler
	// depsFor resuelve los servicios del tenant por request. Por defecto usa
	// events+core; los tests lo reemplazan con fakes para ejercitar el
	// transporte completo sin base de datos.
	depsFor func(c *gin.Context) (*Deps, bool)
}

// NewModule construye el servidor MCP con las herramientas del dominio.
func NewModule(events EventsResolver, core CoreResolver) *Module {
	m := &Module{events: events, core: core}

	server := mcpapi.NewServer(&mcpapi.Implementation{
		Name:    "synapta",
		Version: "1.0.0",
	}, nil)

	mcpapi.AddTool(server, &mcpapi.Tool{
		Name:        "create_project",
		Description: "Create a new Synapta project (an event: name, optional venue and description). Returns the created project with its id.",
	}, m.createProject)
	mcpapi.AddTool(server, &mcpapi.Tool{
		Name:        "list_projects",
		Description: "List the Synapta projects of the current tenant, newest first. By default only active projects; set all=true to include archived ones.",
	}, m.listProjects)
	mcpapi.AddTool(server, &mcpapi.Tool{
		Name:        "list_models",
		Description: "List the generation models available in the catalog. Filter by modality: video, image, audio or text. Use the returned name as the model of generate_video.",
	}, m.listModels)
	mcpapi.AddTool(server, &mcpapi.Tool{
		Name: "generate_video",
		Description: "Queue a video generation in a Synapta project. Requires an event_id (see list_projects) and a text prompt. " +
			"Model is optional (defaults to the first catalog video model). If no piece_id is given, a piece is created or reused inside the project. " +
			"Returns the task id and status; poll list_generations to follow it.",
	}, m.generateVideo)
	mcpapi.AddTool(server, &mcpapi.Tool{
		Name:        "list_generations",
		Description: "List recent generations of the current user (newest first), or poll a single task by task_id. Optionally filter by project (project_id), resource type (video|image|audio|text) and limit.",
	}, m.listGenerations)

	// Stateless + JSON: sin Mcp-Session-Id, cada POST es una sesión
	// temporal — el tenant se resuelve por request vía el bridge de gin.
	m.handler = mcpapi.NewStreamableHTTPHandler(
		func(*http.Request) *mcpapi.Server { return server },
		&mcpapi.StreamableHTTPOptions{Stateless: true, JSONResponse: true},
	)
	m.depsFor = m.resolveDeps
	return m
}

// resolveDeps materializa el bundle del tenant para el request actual.
func (m *Module) resolveDeps(c *gin.Context) (*Deps, bool) {
	eventsHdl, ok := m.events(c)
	if !ok {
		return nil, false
	}
	core, ok := m.core(c)
	if !ok {
		return nil, false
	}
	deps := &Deps{Events: eventsHdl.Service(), Core: core, UserID: utils.UserIDFromContext(c)}
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok {
			deps.Username = s
		}
	}
	return deps, true
}

// Name implementa modules.Module.
func (m *Module) Name() string { return "mcp" }

// Register implementa modules.Module: POST/GET/DELETE /mcp (auth + tenant).
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/mcp")
	g.Use(authMw, tenantMw)
	// GET/DELETE responden 405 con forma MCP (stateless no usa streams ni
	// sesiones) en vez del 404 genérico del router.
	g.POST("", m.serve)
	g.GET("", m.serve)
	g.DELETE("", m.serve)
}

// serve resuelve los servicios del tenant y delega en el handler del SDK.
func (m *Module) serve(c *gin.Context) {
	deps, ok := m.depsFor(c)
	if !ok {
		return
	}
	req := c.Request.WithContext(toolkit.WithDeps(c.Request.Context(), deps))
	m.handler.ServeHTTP(c.Writer, req)
}
