package mcp

import (
	"context"
	"fmt"

	mcpapi "github.com/modelcontextprotocol/go-sdk/mcp"

	"synapta/internal/toolkit"
)

// ─── Aliases al toolkit compartido ──────────────────────────────
//
// Los handlers MCP son una cáscara delgada sobre internal/toolkit: la lógica
// de dominio (anclaje proyecto/pieza, slot de generación, catálogo en vivo)
// vive ahí y también la consume el loop de tool-calling del asistente
// interno (/agency). Los alias conservan los nombres locales que usan los
// tests y el esquema JSON inferido por el SDK (los tags jsonschema viajan
// con el struct).
type (
	EventsAPI         = toolkit.EventsAPI
	AgencyAPI         = toolkit.AgencyAPI
	createProjectIn   = toolkit.CreateProjectIn
	listProjectsIn    = toolkit.ListProjectsIn
	listModelsIn      = toolkit.ListModelsIn
	generateVideoIn   = toolkit.GenerateVideoIn
	listGenerationsIn = toolkit.ListGenerationsIn
	projectOut        = toolkit.ProjectOut
	modelOut          = toolkit.ModelOut
	generateVideoOut  = toolkit.GenerateVideoOut
	generationOut     = toolkit.GenerationOut
)

// ─── Tools ──────────────────────────────────────────────────────

// createProject crea un proyecto (evento) en el tenant autenticado.
func (m *Module) createProject(ctx context.Context, _ *mcpapi.CallToolRequest, in createProjectIn) (*mcpapi.CallToolResult, projectOut, error) {
	deps, err := requireDeps(ctx)
	if err != nil {
		return nil, projectOut{}, err
	}
	out, err := toolkit.CreateProject(ctx, deps, in)
	if err != nil {
		return nil, projectOut{}, err
	}
	return nil, out, nil
}

// listProjects lista los proyectos del tenant (activos por defecto).
func (m *Module) listProjects(ctx context.Context, _ *mcpapi.CallToolRequest, in listProjectsIn) (*mcpapi.CallToolResult, []projectOut, error) {
	deps, err := requireDeps(ctx)
	if err != nil {
		return nil, nil, err
	}
	out, err := toolkit.ListProjects(ctx, deps, in)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// listModels lista el catálogo disponible: modelos de API (estáticos) más
// los downloaded que el worker informa en vivo (nunca cacheados).
func (m *Module) listModels(ctx context.Context, _ *mcpapi.CallToolRequest, in listModelsIn) (*mcpapi.CallToolResult, []modelOut, error) {
	deps, err := requireDeps(ctx)
	if err != nil {
		return nil, nil, err
	}
	out, err := toolkit.ListModels(ctx, deps, in)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// generateVideo encola una generación de video dentro de un proyecto,
// anclándola a una pieza (creándola o reutilizándola, igual que el front).
func (m *Module) generateVideo(ctx context.Context, _ *mcpapi.CallToolRequest, in generateVideoIn) (*mcpapi.CallToolResult, generateVideoOut, error) {
	deps, err := requireDeps(ctx)
	if err != nil {
		return nil, generateVideoOut{}, err
	}
	out, err := toolkit.GenerateVideo(ctx, deps, in)
	if err != nil {
		return nil, generateVideoOut{}, err
	}
	return nil, out, nil
}

// listGenerations lista generaciones recientes del usuario (del proyecto si
// se indica), o con task_id devuelve el estado en vivo de una tarea.
func (m *Module) listGenerations(ctx context.Context, _ *mcpapi.CallToolRequest, in listGenerationsIn) (*mcpapi.CallToolResult, []generationOut, error) {
	deps, err := requireDeps(ctx)
	if err != nil {
		return nil, nil, err
	}
	out, err := toolkit.ListGenerations(ctx, deps, in)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// ─── Helpers ────────────────────────────────────────────────────

func requireDeps(ctx context.Context) (*Deps, error) {
	d, ok := DepsFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("tenant context missing (tool invoked outside the Synapta HTTP bridge)")
	}
	return d, nil
}
