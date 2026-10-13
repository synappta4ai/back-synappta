package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/toolkit"
)

func TestToolsFor(t *testing.T) {
	t.Parallel()

	names := func(step int) []string {
		tools := toolsFor("real_estate", step)
		out := make([]string, 0, len(tools))
		for _, tl := range tools {
			out = append(out, tl.Name)
		}
		return out
	}

	// Paso 0 (chat libre): el set histórico completo, sin skills de flujo.
	wantFree := []string{
		"list_projects", "create_project", "update_project", "list_models",
		"generate_video", "generate_image", "list_generations", "cancel_task",
	}
	if got := names(stepFreeChat); !reflect.DeepEqual(got, wantFree) {
		t.Errorf("paso 0 = %v, want %v", got, wantFree)
	}

	// Paso 1 (análisis): solo analiza — sin generadores.
	wantStep1 := []string{"get_project_brief", "update_project", "ask_user"}
	if got := names(1); !reflect.DeepEqual(got, wantStep1) {
		t.Errorf("paso 1 = %v, want %v", got, wantStep1)
	}

	// Paso 2 (storyboard): generate_image, nunca generate_video.
	has := func(step int, tool string) bool {
		for _, n := range names(step) {
			if n == tool {
				return true
			}
		}
		return false
	}
	if !has(2, "generate_image") || has(2, "generate_video") {
		t.Errorf("paso 2 = %v", names(2))
	}
	// Paso 3 (escenas): generate_video, nunca generate_image.
	if !has(3, "generate_video") || has(3, "generate_image") {
		t.Errorf("paso 3 = %v", names(3))
	}
	// Pasos 1–3 comparten brief + ask_user.
	for step := 1; step <= stepCount; step++ {
		if !has(step, "get_project_brief") || !has(step, "ask_user") {
			t.Errorf("paso %d sin skills de flujo: %v", step, names(step))
		}
	}

	// Los tres agentes usan la misma sublista hoy (la diferencia es el
	// system prompt); el registro no puede filtrar nombres inexistentes.
	for _, wf := range []string{workflowCommercial, workflowRealEstate, workflowCinema} {
		for step := 0; step <= stepCount; step++ {
			if len(toolsFor(wf, step)) != len(stepTools(step)) {
				t.Errorf("%s paso %d perdió tools: %v", wf, step, toolsFor(wf, step))
			}
		}
	}
}

func TestAskUserTool(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	var askDef *toolDef
	for _, tl := range toolsFor(workflowCinema, 1) {
		if tl.Name == "ask_user" {
			askDef = &tl
			break
		}
	}
	if askDef == nil {
		t.Fatal("ask_user no está en el paso 1")
	}

	ctx := toolkit.WithDeps(context.Background(), &toolkit.Deps{UserID: 7, Agent: "cinema"})
	var events []toolEvent
	args := json.RawMessage(`{"questions":["¿Cuál es el desenlace?", "¿Qué registro querés? (cine o comercial)"]}`)
	result, ev, err := executeTool(ctx, toolsFor(workflowCinema, 1), toolCall{Name: "ask_user", Args: args}, func(e toolEvent) {
		events = append(events, e)
	})

	var ask errAskUser
	if !errors.As(err, &ask) {
		t.Fatalf("err = %v, quiero errAskUser", err)
	}
	if len(ask.Questions) != 2 || ask.Questions[0] != "¿Cuál es el desenlace?" {
		t.Errorf("questions = %v", ask.Questions)
	}
	// Terminar el turno con preguntas no es un fallo: evento ok con resumen.
	if ev.Status != "ok" || ev.Name != "ask_user" {
		t.Errorf("event = %+v", ev)
	}
	if len(events) == 0 {
		t.Error("onEvent no fue invocado")
	}
	if result == "" {
		t.Error("result vacío: el loop necesita un tool result que devolver")
	}

	// Sin preguntas no cierra el turno: es un error de la herramienta.
	_, _, err = executeTool(ctx, toolsFor(workflowCinema, 1), toolCall{Name: "ask_user", Args: json.RawMessage(`{"questions":[]}`)}, nil)
	if err == nil || errors.As(err, &ask) {
		t.Errorf("questions vacías err = %v", err)
	}

	// runToolLoop propaga errAskUser (mockea el LLM devolviendo el call).
}

func TestGetProjectBriefTool(t *testing.T) {
	t.Parallel()

	stored := &toolkit.FlowRecord{
		ID:     "flow-1",
		UserID: 7,
		Agent:  "cinema",
		State: map[string]any{
			"step":  float64(1),
			"brief": map[string]any{"guion": "escena A"},
		},
	}
	deps := &toolkit.Deps{UserID: 7, Agent: "cinema", Flows: &briefFlows{stored: stored}}

	var briefDef *toolDef
	for _, tl := range toolsFor(workflowCinema, 1) {
		if tl.Name == "get_project_brief" {
			briefDef = &tl
			break
		}
	}
	if briefDef == nil {
		t.Fatal("get_project_brief no está en el paso 1")
	}

	out, err := briefDef.Run(context.Background(), deps, nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	brief, ok := out.(toolkit.ProjectBriefOut)
	if !ok {
		t.Fatalf("out = %T", out)
	}
	if brief.Agent != "cinema" || brief.Step != 1 || brief.Brief["guion"] != "escena A" {
		t.Errorf("brief = %+v", brief)
	}
}

// briefFlows es un FlowsAPI mínimo para el test de la skill.
type briefFlows struct {
	stored *toolkit.FlowRecord
}

func (f *briefFlows) GetFlow(int64, string) (*toolkit.FlowRecord, error) { return f.stored, nil }

func (f *briefFlows) SaveFlow(toolkit.FlowRecord) error { return nil }
