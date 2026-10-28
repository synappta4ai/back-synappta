// flows.go: persistencia del estado del flujo /agency por (usuario,
// agente) sobre la tabla agency_flows del schema del tenant (schema-
// per-tenant: el search_path del pool ya apunta al schema propio).
//
// El toolkit expone la interfaz testeable (FlowsAPI) que el runtime cablea
// en Deps.Flows; el loop del asistente la usa vía la skill get_project_brief
// y el hook post-validación de los artefactos (SaveArtifact), y el servidor
// MCP la expondrá como tools en la Fase 4 (ver docs/plan-flujos-agentes.md
// §2.4 y §2.5).
package toolkit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ─── Interfaz ──────────────────────────────────────────────────

// FlowsAPI es la superficie de persistencia del flujo /agency que usan el
// chat del asistente y las skills. Satisfacida por *PGFlows en producción
// y por fakes en tests.
type FlowsAPI interface {
	// GetFlow devuelve el flujo del usuario+agente; (nil, nil) si aún no
	// existe (flujo nuevo).
	GetFlow(userID int64, agent string) (*FlowRecord, error)
	// SaveFlow crea o actualiza el flujo completo del usuario+agente
	// (upsert por la clave única (user_id, agent)).
	SaveFlow(rec FlowRecord) error
}

// FlowRecord es el estado del flujo /agency de un usuario y agente.
type FlowRecord struct {
	ID        string
	UserID    int64
	Agent     string
	ProjectID string // "" si todavía no hay proyecto anclado
	State     map[string]any
	UpdatedAt time.Time
}

// ─── Operaciones de dominio ────────────────────────────────────

// GetFlowIn lee el flujo del usuario actual para un agente.
type GetFlowIn struct {
	Agent string `json:"agent" jsonschema:"agent of the flow: commercial, real_estate or cinema (required)"`
}

// SaveFlowIn guarda el estado completo del flujo (reemplazo total del
// state; el front es dueño del objeto).
type SaveFlowIn struct {
	Agent     string         `json:"agent" jsonschema:"agent of the flow (required)"`
	ProjectID string         `json:"project_id,omitempty" jsonschema:"project (event) id anchoring the flow, if any"`
	State     map[string]any `json:"state" jsonschema:"full flow state: {step, brief, artifacts, conversation_id}"`
}

// SaveArtifactIn es el hook post-validación de un artefacto de paso: lo
// mergea en el state del flujo sin pisar los artefactos previos.
type SaveArtifactIn struct {
	Agent          string         `json:"agent"`
	Step           int            `json:"step"`
	Kind           string         `json:"kind"`
	Artifact       map[string]any `json:"artifact"`
	ProjectID      string         `json:"project_id,omitempty"`
	Brief          map[string]any `json:"brief,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
}

// FlowOut es el flujo devuelto a los consumidores.
type FlowOut struct {
	Agent     string         `json:"agent"`
	Step      int            `json:"step,omitempty"`
	ProjectID string         `json:"project_id,omitempty"`
	Brief     map[string]any `json:"brief,omitempty"`
	// Artifacts es el mapa kind → artefacto del state (puede ser nil).
	Artifacts      map[string]any `json:"artifacts,omitempty"`
	ConversationID string         `json:"conversation_id,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at,omitempty"`
}

// GetFlow devuelve el flujo del usuario actual para un agente; un usuario
// sin flujo obtiene state vacío (nunca un error: retomar es opcional).
func GetFlow(ctx context.Context, deps *Deps, in GetFlowIn) (FlowOut, error) {
	_ = ctx
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		return FlowOut{}, fmt.Errorf("agent is required")
	}
	rec, err := requireFlows(deps).GetFlow(deps.UserID, agent)
	if err != nil {
		return FlowOut{}, fmt.Errorf("get flow: %w", err)
	}
	if rec == nil {
		return FlowOut{Agent: agent, Brief: map[string]any{}, Artifacts: map[string]any{}}, nil
	}
	return flowOutFrom(rec), nil
}

// SaveFlow guarda el estado completo del flujo del usuario actual.
func SaveFlow(_ context.Context, deps *Deps, in SaveFlowIn) error {
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		return fmt.Errorf("agent is required")
	}
	state := in.State
	if state == nil {
		state = map[string]any{}
	}
	return requireFlows(deps).SaveFlow(FlowRecord{
		ID:        uuid.NewString(),
		UserID:    deps.UserID,
		Agent:     agent,
		ProjectID: strings.TrimSpace(in.ProjectID),
		State:     state,
	})
}

// SaveArtifact mergea el artefacto validado de un paso en el state del
// flujo (state.step, state.artifacts[kind], state.conversation_id) sin
// pisar los artefactos de otros pasos. Si el request traía brief (paso 1)
// también se persiste.
func SaveArtifact(_ context.Context, deps *Deps, in SaveArtifactIn) error {
	agent := strings.TrimSpace(in.Agent)
	if agent == "" {
		return fmt.Errorf("agent is required")
	}
	if in.Kind == "" || in.Artifact == nil {
		return fmt.Errorf("kind and artifact are required")
	}
	flows := requireFlows(deps)
	rec, err := flows.GetFlow(deps.UserID, agent)
	if err != nil {
		return fmt.Errorf("load flow: %w", err)
	}
	if rec == nil {
		rec = &FlowRecord{ID: uuid.NewString(), UserID: deps.UserID, Agent: agent}
	}
	rec.State = mergeFlowState(rec.State, in)
	if in.ProjectID != "" {
		rec.ProjectID = in.ProjectID
	}
	return flows.SaveFlow(*rec)
}

// mergeFlowState devuelve el state del flujo con el artefacto del paso
// mergeado: conserva artifacts de otros pasos, fija el step actual y el
// conversation_id; el brief solo se escribe si vino en el request.
func mergeFlowState(prev map[string]any, in SaveArtifactIn) map[string]any {
	out := make(map[string]any, len(prev)+3)
	for k, v := range prev {
		out[k] = v
	}
	out["step"] = float64(in.Step)
	artifacts, _ := out["artifacts"].(map[string]any)
	merged := make(map[string]any, len(artifacts)+1)
	for k, v := range artifacts {
		merged[k] = v
	}
	merged[in.Kind] = in.Artifact
	out["artifacts"] = merged
	if in.Brief != nil {
		out["brief"] = in.Brief
	}
	if in.ConversationID != "" {
		out["conversation_id"] = in.ConversationID
	}
	return out
}

// ProjectBriefOut es lo que la skill get_project_brief le devuelve al LLM:
// el brief del paso 0 y el estado del flujo, más el proyecto si ya está
// anclado.
type ProjectBriefOut struct {
	Agent     string         `json:"agent"`
	Step      int            `json:"step,omitempty"`
	ProjectID string         `json:"project_id,omitempty"`
	Brief     map[string]any `json:"brief,omitempty"`
	Artifacts map[string]any `json:"artifacts,omitempty"`
	Project   *ProjectOut    `json:"project,omitempty"`
}

// GetProjectBrief devuelve el brief y el estado del flujo del agente para
// el usuario actual (lo que el paso 1–3 necesita para no repetir preguntas
// ya respondidas). Sin flow guardado devuelve brief vacío, no error.
func GetProjectBrief(ctx context.Context, deps *Deps) (ProjectBriefOut, error) {
	agent := strings.TrimSpace(deps.Agent)
	if agent == "" {
		return ProjectBriefOut{}, fmt.Errorf("agent context missing: get_project_brief only works inside an agency flow")
	}
	flow, err := GetFlow(ctx, deps, GetFlowIn{Agent: agent})
	if err != nil {
		return ProjectBriefOut{}, err
	}
	out := ProjectBriefOut{
		Agent:     flow.Agent,
		Step:      flow.Step,
		ProjectID: flow.ProjectID,
		Brief:     flow.Brief,
		Artifacts: flow.Artifacts,
	}
	if flow.ProjectID != "" && deps.Events != nil {
		if ev, err := deps.Events.GetEvent(flow.ProjectID); err == nil && ev != nil {
			p := projectFromEvent(ev)
			out.Project = &p
		}
	}
	return out, nil
}

// requireFlows devuelve el store del tenant o un error claro si el runtime
// no lo cableó.
func requireFlows(deps *Deps) FlowsAPI {
	if deps == nil || deps.Flows == nil {
		return nilFlows{}
	}
	return deps.Flows
}

// nilFlows responde como un store vacío cuando el bundle no trae Flows
// (p. ej. tests viejos): las lecturas dan vacío y las escrituras un error
// explícito, nunca un panic.
type nilFlows struct{}

func (nilFlows) GetFlow(int64, string) (*FlowRecord, error) { return nil, nil }

func (nilFlows) SaveFlow(FlowRecord) error {
	return fmt.Errorf("flow store not available")
}

// flowOutFrom convierte un record interno a la salida de dominio.
func flowOutFrom(rec *FlowRecord) FlowOut {
	out := FlowOut{
		Agent:     rec.Agent,
		ProjectID: rec.ProjectID,
		UpdatedAt: rec.UpdatedAt,
	}
	if conv, ok := rec.State["conversation_id"].(string); ok {
		out.ConversationID = conv
	}
	if step, ok := rec.State["step"].(float64); ok {
		out.Step = int(step)
	}
	if brief, ok := rec.State["brief"].(map[string]any); ok {
		out.Brief = brief
	} else {
		out.Brief = map[string]any{}
	}
	if artifacts, ok := rec.State["artifacts"].(map[string]any); ok {
		out.Artifacts = artifacts
	} else {
		out.Artifacts = map[string]any{}
	}
	return out
}

// ─── Implementación SQL ────────────────────────────────────────

// PGFlows implementa FlowsAPI sobre el pool del tenant.
type PGFlows struct {
	db *sql.DB
}

// NewPGFlows crea el store sobre el pool del schema del tenant.
func NewPGFlows(db *sql.DB) *PGFlows { return &PGFlows{db: db} }

// GetFlow implements FlowsAPI.
func (s *PGFlows) GetFlow(userID int64, agent string) (*FlowRecord, error) {
	row := s.db.QueryRow(`
		SELECT id, user_id, agent, COALESCE(project_id::text, ''), state, updated_at
		FROM agency_flows
		WHERE user_id = $1 AND agent = $2`, userID, agent)
	var rec FlowRecord
	var raw []byte
	if err := row.Scan(&rec.ID, &rec.UserID, &rec.Agent, &rec.ProjectID, &raw, &rec.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query agency_flows: %w", err)
	}
	rec.State = map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &rec.State); err != nil {
			return nil, fmt.Errorf("parse agency_flows.state: %w", err)
		}
	}
	return &rec, nil
}

// SaveFlow implements FlowsAPI: upsert por (user_id, agent). Un project_id
// vacío no pisa el existente (el flujo anclado se conserva).
func (s *PGFlows) SaveFlow(rec FlowRecord) error {
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	state := rec.State
	if state == nil {
		state = map[string]any{}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal agency_flows.state: %w", err)
	}
	var projectID any
	if v := strings.TrimSpace(rec.ProjectID); v != "" {
		projectID = v
	}
	if _, err := s.db.Exec(`
		INSERT INTO agency_flows (id, user_id, agent, project_id, state)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, agent) DO UPDATE
		SET project_id = COALESCE(EXCLUDED.project_id, agency_flows.project_id),
		    state      = EXCLUDED.state,
		    updated_at = CURRENT_TIMESTAMP`,
		rec.ID, rec.UserID, rec.Agent, projectID, raw,
	); err != nil {
		return fmt.Errorf("upsert agency_flows: %w", err)
	}
	return nil
}
