// Package toolkit reúne las operaciones de dominio que comparte el servidor
// MCP remoto y el loop de tool-calling del asistente interno (/agency):
// proyectos (eventos), catálogo de modelos y generaciones.
//
// Todo recibe los servicios del tenant como argumento (Deps): no hay estado
// global ni acceso a otros tenants — cada bundle resuelto por el middleware
// trae su propio pool de base de datos (schema-per-tenant), de modo que el
// aislamiento lo garantiza el runtime, no esta capa.
package toolkit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/event"
	"synapta/internal/modules/model"
)

// ─── Interfaces de dominio ──────────────────────────────────────
//
// Los consumidores hablan contra estas interfaces (satisfacidas por
// *event.Service y *agency.Core del bundle del tenant) para que la lógica
// sea testeable con fakes sin tocar la base.

// EventsAPI es la superficie del dominio de eventos que usa el toolkit.
type EventsAPI interface {
	CreateEvent(req *event.CreateEventRequest) (*event.Event, error)
	ListEvents(activeOnly bool) ([]event.Event, error)
	GetEvent(id string) (*event.Event, error)
	UpdateEvent(id string, req *event.UpdateEventRequest) (*event.Event, error)
	ListPieces(eventID, programID string) ([]event.Piece, error)
	CreatePiece(eventID string, req *event.CreatePieceRequest) (*event.Piece, error)
	GetPiece(id string) (*event.Piece, error)
	ListGenerations(pieceID string) ([]event.PieceGeneration, error)
}

// AgencyAPI es la superficie del core de generación que usa el toolkit.
type AgencyAPI interface {
	GenerateUnified(req *agency.GenerateRequest) (*agency.GenerateResponse, error)
	UserHistoryForUser(userID int64, f agency.UserHistoryFilter) ([]agency.GenerationLog, error)
	GetStatusUnified(taskID string) (*agency.StatusResponse, error)
	CancelTask(taskID string) error
}

// Deps son los servicios del tenant resueltos para la operación actual, más
// la identidad del usuario que la pide (para el tracking de generaciones)
// y el agente del flujo /agency que atiende el request.
type Deps struct {
	Events   EventsAPI
	Core     AgencyAPI
	Flows    FlowsAPI
	UserID   int64
	Username string
	// Agent es el agente del flujo /agency (commercial | real_estate |
	// cinema); vacío fuera de un turno de flujo. Lo usan las skills
	// get_project_brief / save_artifact para ubicar agency_flows.
	Agent string
}

type depsKey struct{}

// WithDeps inyecta los servicios del tenant en el ctx del request: los
// consumidores (bridge MCP, loop del asistente) los leen con DepsFrom.
func WithDeps(ctx context.Context, deps *Deps) context.Context {
	return context.WithValue(ctx, depsKey{}, deps)
}

// DepsFrom extrae los servicios del tenant del ctx; false si el request no
// vino por un bridge que los cableara.
func DepsFrom(ctx context.Context) (*Deps, bool) {
	d, ok := ctx.Value(depsKey{}).(*Deps)
	return d, ok && d != nil
}

// ─── Proyectos ──────────────────────────────────────────────────

// CreateProjectIn crea un proyecto (evento) en el tenant.
type CreateProjectIn struct {
	Name        string `json:"name" jsonschema:"project name (required)"`
	Venue       string `json:"venue,omitempty" jsonschema:"venue or location of the event"`
	Description string `json:"description,omitempty" jsonschema:"short description of the project"`
}

// ListProjectsIn lista proyectos; por defecto solo los activos.
type ListProjectsIn struct {
	All bool `json:"all,omitempty" jsonschema:"include inactive/archived projects (default: active only)"`
}

// UpdateProjectIn actualiza campos de un proyecto existente (solo los
// seteados se mandan al dominio).
type UpdateProjectIn struct {
	ID          string `json:"id" jsonschema:"project (event) id to update (required)"`
	Name        string `json:"name,omitempty" jsonschema:"new project name"`
	Venue       string `json:"venue,omitempty" jsonschema:"new venue or location"`
	Description string `json:"description,omitempty" jsonschema:"new short description"`
	Status      string `json:"status,omitempty" jsonschema:"new status (e.g. active, archived)"`
}

// ProjectOut es el proyecto devuelto a los consumidores.
type ProjectOut struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Venue       string    `json:"venue,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateProject crea un proyecto en el tenant autenticado.
func CreateProject(ctx context.Context, deps *Deps, in CreateProjectIn) (ProjectOut, error) {
	_ = ctx
	if strings.TrimSpace(in.Name) == "" {
		return ProjectOut{}, fmt.Errorf("name is required")
	}
	ev, err := deps.Events.CreateEvent(&event.CreateEventRequest{
		Name:        strings.TrimSpace(in.Name),
		Venue:       strings.TrimSpace(in.Venue),
		Description: strings.TrimSpace(in.Description),
	})
	if err != nil {
		return ProjectOut{}, fmt.Errorf("create project: %w", err)
	}
	return projectFromEvent(ev), nil
}

// ListProjects lista los proyectos del tenant (activos por defecto).
func ListProjects(_ context.Context, deps *Deps, in ListProjectsIn) ([]ProjectOut, error) {
	events, err := deps.Events.ListEvents(!in.All)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	out := make([]ProjectOut, 0, len(events))
	for i := range events {
		out = append(out, projectFromEvent(&events[i]))
	}
	return out, nil
}

// UpdateProject actualiza campos de un proyecto; los campos vacíos no se
// tocan.
func UpdateProject(_ context.Context, deps *Deps, in UpdateProjectIn) (ProjectOut, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return ProjectOut{}, fmt.Errorf("id is required")
	}
	req := &event.UpdateEventRequest{}
	if v := strings.TrimSpace(in.Name); v != "" {
		req.Name = &v
	}
	if v := strings.TrimSpace(in.Venue); v != "" {
		req.Venue = &v
	}
	if v := strings.TrimSpace(in.Description); v != "" {
		req.Description = &v
	}
	if v := strings.TrimSpace(in.Status); v != "" {
		req.Status = &v
	}
	ev, err := deps.Events.UpdateEvent(id, req)
	if err != nil {
		return ProjectOut{}, fmt.Errorf("update project: %w", err)
	}
	return projectFromEvent(ev), nil
}

// ─── Modelos ────────────────────────────────────────────────────

// ListModelsIn filtra el catálogo por modalidad.
type ListModelsIn struct {
	Modality string `json:"modality,omitempty" jsonschema:"filter by modality: video, image, audio or text"`
}

// ModelOut es una entrada del catálogo.
type ModelOut struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Modality    string `json:"modality"`
	Type        string `json:"type"`
	Available   *bool  `json:"available,omitempty"` // downloaded: lo que informa el worker
}

// ListModels lista el catálogo disponible: modelos de API (estáticos) más
// los downloaded que el worker informa en vivo (nunca cacheados).
func ListModels(ctx context.Context, _ *Deps, in ListModelsIn) ([]ModelOut, error) {
	if in.Modality != "" && !model.IsValidModality(in.Modality) {
		return nil, fmt.Errorf("invalid modality %q (use video, image, audio or text)", in.Modality)
	}
	static := model.List(model.Modality(in.Modality))
	out := make([]ModelOut, 0, len(static))
	for _, mm := range static {
		out = append(out, ModelOut{
			Name:        mm.Name,
			DisplayName: mm.DisplayName,
			Modality:    string(mm.Modality),
			Type:        string(mm.Type),
		})
	}
	// Modelos downloaded: se leen en vivo al worker; si no responde, simplemente
	// no aparecen (regla del repo: nunca inventar ni cachear).
	if addr := model.WorkerAddr(); addr != "" {
		if live, err := model.ListLive(ctx, addr); err == nil {
			for _, mm := range live {
				if in.Modality != "" && string(mm.Modality) != in.Modality {
					continue
				}
				avail := mm.Downloaded != nil && mm.Downloaded.Available
				out = append(out, ModelOut{
					Name:        mm.Name,
					DisplayName: mm.DisplayName,
					Modality:    string(mm.Modality),
					Type:        string(mm.Type),
					Available:   &avail,
				})
			}
		}
	}
	return out, nil
}

// ─── Generaciones ───────────────────────────────────────────────

// GenerateVideoIn encola una generación de video (o de otra modalidad vía
// Generate con ResourceType).
type GenerateVideoIn struct {
	ProjectID   string  `json:"project_id,omitempty" jsonschema:"existing project (event) id; omit to find/create by project_name"`
	ProjectName string  `json:"project_name,omitempty" jsonschema:"find or create a project with this name when project_id is omitted (default: 'MCP')"`
	Prompt      string  `json:"prompt" jsonschema:"text prompt describing the video to generate (required)"`
	Model       string  `json:"model" jsonschema:"generation model name (required); use list_models to discover names"`
	PieceID     string  `json:"piece_id,omitempty" jsonschema:"existing piece id inside the project; omit to find/create by piece_code"`
	PieceCode   string  `json:"piece_code,omitempty" jsonschema:"piece code to find or create when piece_id is omitted (default: 'MCP')"`
	Ratio       string  `json:"ratio,omitempty" jsonschema:"aspect ratio, e.g. 16:9, 9:16, 1:1"`
	Duration    float64 `json:"duration,omitempty" jsonschema:"video duration in seconds (default 5)"`
	Resolution  string  `json:"resolution,omitempty" jsonschema:"output resolution, e.g. 720p, 1080p"`
	Quantity    int     `json:"quantity,omitempty" jsonschema:"number of variants to generate"`
	//nolint:tagliatelle // el contrato del dominio usa snake_case.
	GenerateAudio     *bool    `json:"generate_audio,omitempty" jsonschema:"ask the model to generate audio track"`
	CameraFixed       *bool    `json:"camera_fixed,omitempty" jsonschema:"keep the camera fixed"`
	Seed              string   `json:"seed,omitempty" jsonschema:"seed for reproducible output"`
	Quality           string   `json:"quality,omitempty" jsonschema:"quality preset, when the model supports one"`
	ReferenceImageIDs []string `json:"reference_image_ids,omitempty" jsonschema:"file ids (UUIDs) of reference images already uploaded to Synapta"`
}

// GenerateImageIn encola una generación de imagen (hojas de storyboard,
// viñetas, fotos guía) con el mismo anclaje proyecto/pieza que el video.
type GenerateImageIn struct {
	ProjectID   string `json:"project_id,omitempty" jsonschema:"existing project (event) id; omit to find/create by project_name"`
	ProjectName string `json:"project_name,omitempty" jsonschema:"find or create a project with this name when project_id is omitted"`
	Prompt      string `json:"prompt" jsonschema:"text prompt describing the image to generate (required)"`
	Model       string `json:"model" jsonschema:"image generation model name (required); use list_models to discover names"`
	PieceID     string `json:"piece_id,omitempty" jsonschema:"existing piece id inside the project; omit to find/create by piece_code"`
	PieceCode   string `json:"piece_code,omitempty" jsonschema:"piece code to find or create when piece_id is omitted"`
	Ratio       string `json:"ratio,omitempty" jsonschema:"aspect ratio, e.g. 16:9, 9:16, 1:1"`
	Resolution  string `json:"resolution,omitempty" jsonschema:"output resolution, e.g. 720p, 1080p"`
	Quantity    int    `json:"quantity,omitempty" jsonschema:"number of variants to generate"`
	Seed        string `json:"seed,omitempty" jsonschema:"seed for reproducible output"`
	Quality     string `json:"quality,omitempty" jsonschema:"quality preset, when the model supports one"`
	//nolint:tagliatelle // el contrato del dominio usa snake_case.
	ReferenceImageIDs []string `json:"reference_image_ids,omitempty" jsonschema:"file ids (UUIDs) of reference images already uploaded to Synapta"`
}

// GenerateVideoOut es la respuesta de una generación encolada.
type GenerateVideoOut struct {
	TaskID           string  `json:"task_id"`
	Status           string  `json:"status"`
	Model            string  `json:"model"`
	EventID          string  `json:"event_id"`
	EventName        string  `json:"event_name,omitempty"`
	PieceID          string  `json:"piece_id"`
	PieceCode        string  `json:"piece_code"`
	GenerationNumber int     `json:"generation_number"`
	CostCredits      float64 `json:"cost_credits,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
}

// GenerateVideo encola una generación de video dentro de un proyecto,
// anclándola a una pieza (creándola o reutilizándola, igual que el front).
func GenerateVideo(ctx context.Context, deps *Deps, in GenerateVideoIn) (GenerateVideoOut, error) {
	return generate(ctx, deps, "video", generateSpec{
		ProjectID:       in.ProjectID,
		ProjectName:     in.ProjectName,
		Prompt:          in.Prompt,
		Model:           in.Model,
		PieceID:         in.PieceID,
		PieceCode:       in.PieceCode,
		Ratio:           in.Ratio,
		Duration:        in.Duration,
		Resolution:      in.Resolution,
		Quantity:        in.Quantity,
		GenerateAudio:   in.GenerateAudio,
		CameraFixed:     in.CameraFixed,
		Seed:            in.Seed,
		Quality:         in.Quality,
		ReferenceImages: in.ReferenceImageIDs,
	})
}

// GenerateImage encola una generación de imagen con el mismo anclaje que el
// video (proyecto + pieza + slot de generación).
func GenerateImage(ctx context.Context, deps *Deps, in GenerateImageIn) (GenerateVideoOut, error) {
	return generate(ctx, deps, "image", generateSpec{
		ProjectID:       in.ProjectID,
		ProjectName:     in.ProjectName,
		Prompt:          in.Prompt,
		Model:           in.Model,
		PieceID:         in.PieceID,
		PieceCode:       in.PieceCode,
		Ratio:           in.Ratio,
		Resolution:      in.Resolution,
		Quantity:        in.Quantity,
		Seed:            in.Seed,
		Quality:         in.Quality,
		ReferenceImages: in.ReferenceImageIDs,
	})
}

// generateSpec es el denominador común entre video e imagen.
type generateSpec struct {
	ProjectID       string
	ProjectName     string
	Prompt          string
	Model           string
	PieceID         string
	PieceCode       string
	Ratio           string
	Duration        float64
	Resolution      string
	Quantity        int
	GenerateAudio   *bool
	CameraFixed     *bool
	Seed            string
	Quality         string
	ReferenceImages []string
}

// generate ancla proyecto + pieza y encola la generación en el core del
// tenant. resourceType ("video"/"image") queda en el log de generaciones.
func generate(_ context.Context, deps *Deps, resourceType string, spec generateSpec) (GenerateVideoOut, error) {
	if strings.TrimSpace(spec.Prompt) == "" {
		return GenerateVideoOut{}, fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(spec.Model) == "" {
		return GenerateVideoOut{}, fmt.Errorf("model is required (use list_models to discover names)")
	}

	// 1. Proyecto: por id, o por nombre (find-or-create).
	project, err := resolveProject(deps.Events, spec.ProjectID, spec.ProjectName)
	if err != nil {
		return GenerateVideoOut{}, err
	}

	// 2. Pieza: por id, o por código (find-or-create).
	piece, err := resolvePiece(deps.Events, project.ID, spec.PieceID, spec.PieceCode)
	if err != nil {
		return GenerateVideoOut{}, err
	}

	// 3. Slot de generación: el siguiente número libre de la pieza, para no
	// pisar tomas anteriores (SaveGenerationOutput desactiva el mismo número).
	genNumber, err := NextGenerationNumber(deps.Events, piece.ID)
	if err != nil {
		return GenerateVideoOut{}, err
	}

	// 4. Contenido: prompt + imágenes de referencia ya subidas (por file id).
	content := []agency.ContentItem{{Type: "text", Text: strings.TrimSpace(spec.Prompt)}}
	for _, id := range spec.ReferenceImages {
		id = strings.TrimSpace(id)
		if id != "" {
			content = append(content, agency.ContentItem{Type: "image", ID: id})
		}
	}

	duration := spec.Duration
	if duration <= 0 {
		duration = 5 // mismo default del formulario de video del front
	}

	req := &agency.GenerateRequest{
		Model:            strings.TrimSpace(spec.Model),
		Content:          content,
		Ratio:            strings.TrimSpace(spec.Ratio),
		Duration:         duration,
		Resolution:       strings.TrimSpace(spec.Resolution),
		Quantity:         spec.Quantity,
		GenerateAudio:    spec.GenerateAudio,
		CameraFixed:      spec.CameraFixed,
		Seed:             strings.TrimSpace(spec.Seed),
		Quality:          strings.TrimSpace(spec.Quality),
		EventID:          project.ID,
		EventName:        project.Name,
		PieceID:          piece.ID,
		PieceCode:        piece.PieceCode,
		GenerationNumber: genNumber,
		UserID:           int(deps.UserID),
		UserName:         deps.Username,
		ResourceType:     resourceType,
	}

	result, err := deps.Core.GenerateUnified(req)
	if err != nil {
		return GenerateVideoOut{}, fmt.Errorf("generate %s: %w", resourceType, err)
	}
	return GenerateVideoOut{
		TaskID:           result.TaskID,
		Status:           result.Status,
		Model:            result.Model,
		EventID:          project.ID,
		EventName:        project.Name,
		PieceID:          piece.ID,
		PieceCode:        piece.PieceCode,
		GenerationNumber: genNumber,
		CostCredits:      result.CostCredits,
		CostUSD:          result.CostUSD,
	}, nil
}

// ─── Historial y estado ─────────────────────────────────────────

// ListGenerationsIn lista generaciones o hace el poll de una tarea.
type ListGenerationsIn struct {
	ProjectID    string `json:"project_id,omitempty" jsonschema:"limit the listing to one project (event)"`
	TaskID       string `json:"task_id,omitempty" jsonschema:"when set, return only the live status of this task (progress and outputs)"`
	ResourceType string `json:"resource_type,omitempty" jsonschema:"filter by type: video, image, audio or text"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max rows to return (default 20, max 100)"`
}

// GenerationOut es una fila del historial (o el estado de una tarea).
type GenerationOut struct {
	TaskID           string                  `json:"task_id"`
	Model            string                  `json:"model,omitempty"`
	Status           string                  `json:"status"`
	Error            string                  `json:"error,omitempty"`
	ProgressPercent  int                     `json:"progress_percent,omitempty"`
	EventID          string                  `json:"event_id,omitempty"`
	PieceCode        string                  `json:"piece_code,omitempty"`
	GenerationNumber int                     `json:"generation_number,omitempty"`
	ResourceType     string                  `json:"resource_type,omitempty"`
	Outputs          []agency.OutputResource `json:"outputs,omitempty"`
	CreatedAt        *time.Time              `json:"created_at,omitempty"`
}

// ListGenerations lista generaciones recientes del usuario (del proyecto si
// se indica), o con task_id devuelve el estado en vivo de una tarea.
func ListGenerations(_ context.Context, deps *Deps, in ListGenerationsIn) ([]GenerationOut, error) {
	// Poll de una tarea: estado en vivo (progreso + outputs).
	if tid := strings.TrimSpace(in.TaskID); tid != "" {
		st, err := deps.Core.GetStatusUnified(tid)
		if err != nil {
			return nil, fmt.Errorf("task status: %w", err)
		}
		return []GenerationOut{{
			TaskID:          tid,
			Status:          st.Status,
			Error:           st.Error,
			ProgressPercent: st.ProgressPercent,
			Outputs:         st.Outputs,
		}}, nil
	}

	limit := in.Limit
	if limit < 1 || limit > 100 {
		limit = 20
	}
	logs, err := deps.Core.UserHistoryForUser(deps.UserID, agency.UserHistoryFilter{
		EventID:      strings.TrimSpace(in.ProjectID),
		ResourceType: strings.TrimSpace(in.ResourceType),
		Limit:        limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	out := make([]GenerationOut, 0, len(logs))
	for i := range logs {
		l := &logs[i]
		out = append(out, GenerationOut{
			TaskID:           l.TaskID,
			Model:            l.ModelName,
			Status:           l.Status,
			Error:            l.ErrorMessage,
			ProgressPercent:  l.Progress,
			EventID:          l.EventID,
			PieceCode:        l.PieceCode,
			GenerationNumber: l.GenerationNumber,
			ResourceType:     l.ResourceType,
			Outputs:          l.Outputs,
			CreatedAt:        &l.CreatedAt,
		})
	}
	return out, nil
}

// CancelTaskIn cancela una generación en curso.
type CancelTaskIn struct {
	TaskID string `json:"task_id" jsonschema:"id of the running task to cancel (required)"`
}

// CancelTask cancela una tarea en curso (si existe y no terminó).
func CancelTask(_ context.Context, deps *Deps, in CancelTaskIn) (string, error) {
	tid := strings.TrimSpace(in.TaskID)
	if tid == "" {
		return "", fmt.Errorf("task_id is required")
	}
	if err := deps.Core.CancelTask(tid); err != nil {
		return "", fmt.Errorf("cancel task: %w", err)
	}
	return "cancelled", nil
}

// ─── Helpers ────────────────────────────────────────────────────

func projectFromEvent(ev *event.Event) ProjectOut {
	return ProjectOut{
		ID:          ev.ID,
		Name:        ev.Name,
		Venue:       ev.Venue,
		Description: ev.Description,
		CreatedAt:   ev.CreatedAt,
	}
}

// resolveProject devuelve el proyecto por id, o bien busca uno con el nombre
// dado y lo crea si no existe (anclaje find-or-create del front).
func resolveProject(ev EventsAPI, projectID, projectName string) (*event.Event, error) {
	if pid := strings.TrimSpace(projectID); pid != "" {
		evFound, err := ev.GetEvent(pid)
		if err != nil {
			return nil, fmt.Errorf("lookup project: %w", err)
		}
		if evFound == nil {
			return nil, fmt.Errorf("project %q not found (use list_projects or create_project)", pid)
		}
		return evFound, nil
	}

	name := strings.TrimSpace(projectName)
	if name == "" {
		name = "MCP"
	}
	existing, err := ev.ListEvents(false)
	if err != nil {
		return nil, fmt.Errorf("lookup project by name: %w", err)
	}
	for i := range existing {
		if existing[i].Name == name {
			return &existing[i], nil
		}
	}
	created, err := ev.CreateEvent(&event.CreateEventRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("create project %q: %w", name, err)
	}
	return created, nil
}

// resolvePiece devuelve la pieza por id, o bien busca una con el código dado
// dentro del proyecto y la crea si no existe.
func resolvePiece(ev EventsAPI, projectID, pieceID, pieceCode string) (*event.Piece, error) {
	if pie := strings.TrimSpace(pieceID); pie != "" {
		pc, err := ev.GetPiece(pie)
		if err != nil {
			return nil, fmt.Errorf("lookup piece: %w", err)
		}
		if pc == nil {
			return nil, fmt.Errorf("piece %q not found", pie)
		}
		if pc.EventID != projectID {
			return nil, fmt.Errorf("piece %q does not belong to project %q", pie, projectID)
		}
		return pc, nil
	}

	code := strings.TrimSpace(pieceCode)
	if code == "" {
		code = "MCP"
	}
	pieces, err := ev.ListPieces(projectID, "")
	if err != nil {
		return nil, fmt.Errorf("lookup piece by code: %w", err)
	}
	for i := range pieces {
		if pieces[i].PieceCode == code {
			return &pieces[i], nil
		}
	}
	pc, err := ev.CreatePiece(projectID, &event.CreatePieceRequest{
		Number:    len(pieces) + 1,
		PieceCode: code,
		Name:      code,
		Type:      "video",
	})
	if err != nil {
		return nil, fmt.Errorf("create piece %q: %w", code, err)
	}
	return pc, nil
}

// NextGenerationNumber devuelve el siguiente slot libre de la pieza (max+1),
// de modo que cada toma nueva guarde su propio número.
func NextGenerationNumber(ev EventsAPI, pieceID string) (int, error) {
	gens, err := ev.ListGenerations(pieceID)
	if err != nil {
		return 0, fmt.Errorf("list piece generations: %w", err)
	}
	max := 0
	for _, g := range gens {
		if g.Number > max {
			max = g.Number
		}
	}
	return max + 1, nil
}
