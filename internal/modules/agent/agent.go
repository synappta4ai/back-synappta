// Package agent proxies the DCS Agent chat (SSE) of the standalone
// agent-server into the Synapta API. The tenant's LLM provider credential is
// resolved from the database and injected as X-LLM-* request headers, so the
// provider key never travels through the browser nor any .env file.
//
// When the agent-server is unreachable the module answers the turn itself by
// calling the tenant's LLM directly (see direct.go), so flows like /agency
// keep working without that extra service.
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/event"
	"synapta/internal/toolkit"
	"synapta/internal/utils"
)

// CredentialResolver supplies the tenant-scoped credential handler per request.
type CredentialResolver func(c *gin.Context) (*credential.Handler, bool)

// EventsResolver supplies the tenant-scoped events handler per request.
type EventsResolver func(c *gin.Context) (*event.Handler, bool)

// CoreResolver supplies the tenant-scoped agency core per request.
type CoreResolver func(c *gin.Context) (*agency.Core, bool)

// chatRequest is the front-end payload for one agent chat turn.
type chatRequest struct {
	ConversationID string `json:"conversation_id"`
	Message        string `json:"message"`
	Provider       string `json:"provider"`
	Workflow       string `json:"workflow,omitempty"`
	// Artifact pide un artefacto estructurado ("sales_angles") en vez de
	// una respuesta conversacional. Vacío = chat normal del experto.
	Artifact string `json:"artifact,omitempty"`
	// Model overrides the LLM configured in admin/models (credential extra)
	// for this single request — lets the agency flow pick the model per step.
	Model string `json:"model,omitempty"`
}

// Module registers the /agent routes (tenant-scoped).
type Module struct {
	resolve  CredentialResolver
	events   EventsResolver
	core     CoreResolver
	agentURL string
	client   *http.Client
	// Historial de turnos del fallback directo (ver direct.go).
	convs *convStore
}

// NewModule creates the proxy module. agentURL points at the DCS Agent
// server (default http://localhost:3200). events/core aportan los servicios
// del tenant que el loop de tool-calling del asistente necesita (mismos
// resolvers del runtime que usa el módulo MCP).
func NewModule(resolve CredentialResolver, events EventsResolver, core CoreResolver, agentURL string) *Module {
	if strings.TrimSpace(agentURL) == "" {
		agentURL = "http://localhost:3200"
	}
	// Sin timeout total (la respuesta es un SSE largo) pero con dial corto:
	// si el agent-server está caído, el fallback al LLM dispara rápido.
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return &Module{
		resolve:  resolve,
		events:   events,
		core:     core,
		agentURL: strings.TrimRight(agentURL, "/"),
		client:   &http.Client{Transport: transport},
		convs:    newConvStore(),
	}
}

// Name implements modules.Module.
func (m *Module) Name() string { return "agent" }

// Register implements modules.Module: POST /agent/chat (auth + tenant).
// Además registra las tres rutas especializadas del flujo /agency, una
// por agente experto; el workflow lo fija la URL, sin depender del body.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/agent")
	g.Use(authMw, tenantMw)
	{
		g.POST("/chat", m.Chat)
		for _, slug := range []string{"commercial", "real-estate", "cinema"} {
			s := slug
			g.POST("/chat/"+s, func(c *gin.Context) { m.ChatWorkflow(c, s) })
		}
	}
}

// ChatWorkflow procesa un turno de una ruta especializada
// (/agent/chat/commercial|real-estate|cinema): el slug de la URL elige
// al experto que atiende el flujo /agency.
func (m *Module) ChatWorkflow(c *gin.Context, slug string) {
	workflow, ok := workflowFromSlug(slug)
	if !ok {
		utils.BadRequest(c, fmt.Sprintf("unknown agent %q", slug))
		return
	}
	m.chatTurn(c, workflow)
}

// Chat forwards one message to the agent server and streams its SSE answer
// back to the client, adding the tenant's LLM credentials as headers. El
// workflow sale del body (ruta legada /agent/chat).
func (m *Module) Chat(c *gin.Context) {
	m.chatTurn(c, "")
}

// chatTurn atiende un turno con el workflow forzado por la ruta (vacío
// = el del body): primero el agent-server, y si no responde el fallback
// directo con la persona experta del workflow.
func (m *Module) chatTurn(c *gin.Context, forcedWorkflow string) {
	hdl, ok := m.resolve(c)
	if !ok {
		return
	}

	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		utils.BadRequest(c, "message is required")
		return
	}
	if forcedWorkflow != "" {
		req.Workflow = forcedWorkflow
	}
	if !credential.IsValidProvider(req.Provider) {
		utils.BadRequest(c, fmt.Sprintf("invalid provider %q", req.Provider))
		return
	}

	cred, err := hdl.Resolve(req.Provider)
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if cred == nil || cred.AuthKey() == "" {
		utils.BadRequest(c, fmt.Sprintf("no credentials stored for provider %q: save them in the flow panel first", req.Provider))
		return
	}

	// Servicios del tenant para el loop de tool-calling del fallback directo.
	// Si el bundle no resuelve, el chat sigue igual sin tools: no se cae el
	// turno por falta de dominio.
	deps := m.tenantDeps(c)

	payload, err := json.Marshal(map[string]string{
		"conversation_id": req.ConversationID,
		"message":         req.Message,
		"workflow":        req.Workflow,
	})
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}

	upstream, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, m.agentURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("X-LLM-Key", cred.AuthKey())
	upstream.Header.Set("X-LLM-Provider", req.Provider)
	if model := strings.TrimSpace(req.Model); model != "" {
		upstream.Header.Set("X-LLM-Model", model)
	} else if model := modelFromExtra(cred.Extra); model != "" {
		upstream.Header.Set("X-LLM-Model", model)
	}
	if cred.BaseURL != "" {
		upstream.Header.Set("X-LLM-BaseURL", cred.BaseURL)
	}

	resp, err := m.client.Do(upstream)
	if err != nil {
		// Agent-server caído (p. ej. localhost:3200 sin proceso): el back
		// responde el turno llamando directo al LLM del tenant.
		if c.Request.Context().Err() != nil {
			return
		}
		m.directChat(c, req, hdl, cred, deps)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		utils.Error(c, http.StatusBadGateway, fmt.Sprintf("agent server returned %s: %s", resp.Status, strings.TrimSpace(string(msg))))
		return
	}

	// Stream the SSE body through, flushing as chunks arrive.
	c.Status(http.StatusOK)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				break
			}
			c.Writer.Flush()
		}
		if readErr != nil {
			break
		}
	}
}

// tenantDeps materializa los servicios del tenant (eventos + core de
// generación) con la identidad del usuario para el loop de tool-calling.
// Devuelve nil si el bundle no está disponible: el turno de chat sigue sin
// tools en vez de fallar.
func (m *Module) tenantDeps(c *gin.Context) *toolkit.Deps {
	if m.events == nil || m.core == nil {
		return nil
	}
	eventsHdl, ok := m.events(c)
	if !ok {
		return nil
	}
	core, ok := m.core(c)
	if !ok {
		return nil
	}
	deps := &toolkit.Deps{
		Events: eventsHdl.Service(),
		Core:   core,
		UserID: utils.UserIDFromContext(c),
	}
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok {
			deps.Username = s
		}
	}
	return deps
}

// modelFromExtra pulls the "model" field out of the credential's extra JSON.
func modelFromExtra(extra string) string {
	if strings.TrimSpace(extra) == "" {
		return ""
	}
	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(extra), &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Model)
}
