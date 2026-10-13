// tools.go: registry de herramientas que el asistente interno (/agency) puede
// invocar con function calling nativo del LLM. Cada tool delega en el toolkit
// compartido (internal/toolkit) con los servicios del tenant del request: el
// mismo dominio que expone el servidor MCP remoto, acá servido al LLM para
// que el usuario conduzca el flujo desde el chat sin tocar formularios.
//
// Los schemas JSON van a mano (formato "parameters" de OpenAI / "input_schema"
// de Anthropic): el SDK no participa en este camino, que habla el API crudo
// de cada provider.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"synapta/internal/toolkit"
)

// stepFreeChat es el "paso" del chat libre del asistente (sin artefacto):
// ve el set histórico completo de herramientas.
const stepFreeChat = 0

// toolEvent notifica al front el avance de cada invocación (SSE "tool").
type toolEvent struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // running | ok | error
	Summary string `json:"summary,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	Error   string `json:"error,omitempty"`
}

// toolDef es una herramienta expuesta al LLM.
type toolDef struct {
	Name        string
	Description string
	// Parameters es el JSON Schema de los argumentos (formato OpenAI
	// "function.parameters" / Anthropic "input_schema").
	Parameters map[string]any
	// Run ejecuta el tool con los argumentos crudos del LLM y devuelve el
	// resultado (se serializa a JSON como tool result). onEvent reporta el
	// avance al front; puede ser nil en tests.
	Run func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, onEvent func(toolEvent)) (any, error)
}

// agentTools devuelve el set de herramientas del asistente. El orden es el
// sugerido al LLM: primero consulta, después escritura.
func agentTools() []toolDef {
	return []toolDef{
		{
			Name:        "list_projects",
			Description: "List the projects (events) of the current tenant, newest first. Default: only active projects.",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"all": map[string]any{"type": "boolean", "description": "include inactive/archived projects"}},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.ListProjectsIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.ListProjects(ctx, deps, in)
			},
		},
		{
			Name:        "create_project",
			Description: "Create a new project (event): name, optional venue and description. Returns the created project with its id.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "project name (required)"},
					"venue":       map[string]any{"type": "string", "description": "venue or location of the event"},
					"description": map[string]any{"type": "string", "description": "short description of the project"},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.CreateProjectIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.CreateProject(ctx, deps, in)
			},
		},
		{
			Name:        "update_project",
			Description: "Update fields of an existing project (only the fields you send are changed).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "description": "project (event) id to update (required)"},
					"name":        map[string]any{"type": "string", "description": "new project name"},
					"venue":       map[string]any{"type": "string", "description": "new venue or location"},
					"description": map[string]any{"type": "string", "description": "new short description"},
					"status":      map[string]any{"type": "string", "description": "new status (e.g. active, archived)"},
				},
				"required":             []string{"id"},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.UpdateProjectIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.UpdateProject(ctx, deps, in)
			},
		},
		{
			Name:        "list_models",
			Description: "List the generation models available in the catalog. Filter by modality: video, image, audio or text. Use the returned name as the model of generate_video / generate_image.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"modality": map[string]any{"type": "string", "description": "filter by modality: video, image, audio or text"},
				},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.ListModelsIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.ListModels(ctx, deps, in)
			},
		},
		{
			Name:        "generate_video",
			Description: "Queue a video generation inside a project. Requires a prompt and a model name (see list_models). If no project_id is given, find or create a project by project_name. Returns the task id; poll list_generations with that task_id to follow it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id":   map[string]any{"type": "string", "description": "existing project (event) id; omit to find/create by project_name"},
					"project_name": map[string]any{"type": "string", "description": "find or create a project with this name when project_id is omitted"},
					"prompt":       map[string]any{"type": "string", "description": "text prompt describing the video to generate (required)"},
					"model":        map[string]any{"type": "string", "description": "generation model name (required); use list_models to discover names"},
					"piece_id":     map[string]any{"type": "string", "description": "existing piece id inside the project; omit to find/create by piece_code"},
					"piece_code":   map[string]any{"type": "string", "description": "piece code to find or create when piece_id is omitted"},
					"ratio":        map[string]any{"type": "string", "description": "aspect ratio, e.g. 16:9, 9:16, 1:1"},
					"duration":     map[string]any{"type": "number", "description": "video duration in seconds (default 5)"},
					"resolution":   map[string]any{"type": "string", "description": "output resolution, e.g. 720p, 1080p"},
					"quantity":     map[string]any{"type": "integer", "description": "number of variants to generate"},
					"generate_audio": map[string]any{
						"type": []any{"null", "boolean"}, "description": "ask the model to generate audio track",
					},
					"camera_fixed": map[string]any{
						"type": []any{"null", "boolean"}, "description": "keep the camera fixed",
					},
					"seed":                map[string]any{"type": "string", "description": "seed for reproducible output"},
					"quality":             map[string]any{"type": "string", "description": "quality preset, when the model supports one"},
					"reference_image_ids": map[string]any{"type": []any{"null", "array"}, "items": map[string]any{"type": "string"}, "description": "file ids (UUIDs) of reference images already uploaded to Synapta"},
				},
				"required":             []string{"prompt", "model"},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, onEvent func(toolEvent)) (any, error) {
				var in toolkit.GenerateVideoIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				out, err := toolkit.GenerateVideo(ctx, deps, in)
				if err != nil {
					return nil, err
				}
				if onEvent != nil {
					onEvent(toolEvent{Name: "generate_video", Status: "ok", TaskID: out.TaskID,
						Summary: fmt.Sprintf("%s · %s · pieza %s #%d", out.EventName, out.Model, out.PieceCode, out.GenerationNumber)})
				}
				return out, nil
			},
		},
		{
			Name:        "generate_image",
			Description: "Queue an image generation (storyboard sheet, shot guide, reference image) inside a project. Requires a prompt and an image model name (see list_models with modality=image). Returns the task id; poll list_generations with that task_id to follow it.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id":   map[string]any{"type": "string", "description": "existing project (event) id; omit to find/create by project_name"},
					"project_name": map[string]any{"type": "string", "description": "find or create a project with this name when project_id is omitted"},
					"prompt":       map[string]any{"type": "string", "description": "text prompt describing the image to generate (required)"},
					"model":        map[string]any{"type": "string", "description": "image generation model name (required); use list_models to discover names"},
					"piece_id":     map[string]any{"type": "string", "description": "existing piece id inside the project; omit to find/create by piece_code"},
					"piece_code":   map[string]any{"type": "string", "description": "piece code to find or create when piece_id is omitted"},
					"ratio":        map[string]any{"type": "string", "description": "aspect ratio, e.g. 16:9, 9:16, 1:1"},
					"resolution":   map[string]any{"type": "string", "description": "output resolution, e.g. 720p, 1080p"},
					"quantity":     map[string]any{"type": "integer", "description": "number of variants to generate"},
					"seed":         map[string]any{"type": "string", "description": "seed for reproducible output"},
					"quality":      map[string]any{"type": "string", "description": "quality preset, when the model supports one"},
					"reference_image_ids": map[string]any{
						"type": []any{"null", "array"}, "items": map[string]any{"type": "string"}, "description": "file ids (UUIDs) of reference images already uploaded to Synapta",
					},
				},
				"required":             []string{"prompt", "model"},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, onEvent func(toolEvent)) (any, error) {
				var in toolkit.GenerateImageIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				out, err := toolkit.GenerateImage(ctx, deps, in)
				if err != nil {
					return nil, err
				}
				if onEvent != nil {
					onEvent(toolEvent{Name: "generate_image", Status: "ok", TaskID: out.TaskID,
						Summary: fmt.Sprintf("%s · %s · pieza %s #%d", out.EventName, out.Model, out.PieceCode, out.GenerationNumber)})
				}
				return out, nil
			},
		},
		{
			Name:        "list_generations",
			Description: "List recent generations of the current user (newest first), or poll a single task by task_id (returns live progress and outputs).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id":    map[string]any{"type": "string", "description": "limit the listing to one project (event)"},
					"task_id":       map[string]any{"type": "string", "description": "when set, return only the live status of this task"},
					"resource_type": map[string]any{"type": "string", "description": "filter by type: video, image, audio or text"},
					"limit":         map[string]any{"type": "integer", "description": "max rows to return (default 20, max 100)"},
				},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.ListGenerationsIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.ListGenerations(ctx, deps, in)
			},
		},
		{
			Name:        "cancel_task",
			Description: "Cancel a running generation task by task_id. Use when the user wants to stop an in-flight generation.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id": map[string]any{"type": "string", "description": "id of the running task to cancel (required)"},
				},
				"required":             []string{"task_id"},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in toolkit.CancelTaskIn
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				status, err := toolkit.CancelTask(ctx, deps, in)
				if err != nil {
					return nil, err
				}
				return map[string]string{"task_id": in.TaskID, "status": status}, nil
			},
		},
	}
}

// errAskUser señala que el LLM invocó ask_user para terminar el turno con
// preguntas al usuario: runToolLoop la propaga sin tratarla como fallo y
// el caller la traduce al evento SSE "questions".
type errAskUser struct{ Questions []string }

func (e errAskUser) Error() string {
	return fmt.Sprintf("%d pregunta(s) para el usuario", len(e.Questions))
}

// flowTools son las skills nuevas del flujo /agency por paso (§2.5 del
// plan): get_project_brief (brief + estado del flujo) y ask_user (cierra
// el turno con preguntas). save_artifact NO es una tool invocable por el
// modelo: es un hook del back post-validación (direct.go).
func flowTools() []toolDef {
	return []toolDef{
		{
			Name:        "get_project_brief",
			Description: "Read the agency flow state of this agent for the current user: the step 0 brief, the artifacts already approved and the linked project. Use it at the start of every artifact step so you never ask again for data that is already there.",
			Parameters: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			Run: func(ctx context.Context, deps *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in struct{}
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				return toolkit.GetProjectBrief(ctx, deps)
			},
		},
		{
			Name:        "ask_user",
			Description: "Ask the user short questions when critical information is missing to continue the step. Ends the turn: the questions are shown in the chat and the user answers in the next message. Every question must carry a concrete suggestion the user can accept.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"questions": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "short questions, each with a suggestion (required)",
					},
				},
				"required":             []string{"questions"},
				"additionalProperties": false,
			},
			Run: func(_ context.Context, _ *toolkit.Deps, args json.RawMessage, _ func(toolEvent)) (any, error) {
				var in struct {
					Questions []string `json:"questions"`
				}
				if err := unmarshalArgs(args, &in); err != nil {
					return nil, err
				}
				qs := make([]string, 0, len(in.Questions))
				for _, q := range in.Questions {
					if s := strings.TrimSpace(q); s != "" {
						qs = append(qs, s)
					}
				}
				if len(qs) == 0 {
					return nil, fmt.Errorf("questions is required")
				}
				return nil, errAskUser{Questions: qs}
			},
		},
	}
}

// toolsFor devuelve las herramientas visibles para un agente en un paso:
// el chat libre (paso 0) ve el set completo de siempre; cada paso de
// artefacto ve solo las skills que su contrato necesita — en análisis solo
// se analiza (sin generate_*), el storyboard usa generate_image y las
// escenas generate_video (§2.5 del plan). Hoy la sublista es la misma para
// los tres agentes: la diferencia entre agentes está en los system prompts
// y en los contratos de artefacto, no en el código de las skills.
func toolsFor(_ string, step int) []toolDef {
	index := make(map[string]toolDef)
	for _, t := range append(agentTools(), flowTools()...) {
		index[t.Name] = t
	}
	names := stepTools(step)
	out := make([]toolDef, 0, len(names))
	for _, n := range names {
		if t, ok := index[n]; ok {
			out = append(out, t)
		}
	}
	return out
}

// stepTools es la lista de skills (en orden) de cada paso.
func stepTools(step int) []string {
	switch step {
	case 1:
		// Análisis: leer el brief, anotar datos y preguntar si falta algo.
		return []string{"get_project_brief", "update_project", "ask_user"}
	case 2:
		// Storyboard: la hoja se genera con imagen.
		return []string{"get_project_brief", "update_project", "ask_user", "list_models", "generate_image", "list_generations", "cancel_task"}
	case 3:
		// Escenas: el clip se genera con video.
		return []string{"get_project_brief", "update_project", "ask_user", "list_models", "generate_video", "list_generations", "cancel_task"}
	default:
		// Chat libre del asistente (paso 0): set histórico completo.
		return []string{"list_projects", "create_project", "update_project", "list_models", "generate_video", "generate_image", "list_generations", "cancel_task"}
	}
}

// unmarshalArgs decodifica los argumentos crudos del LLM; un JSON vacío es
// un objeto vacío válido (tools sin argumentos).
func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return nil
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	return nil
}
