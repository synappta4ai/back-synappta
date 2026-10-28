package toolkit

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeFlows es un FlowsAPI en memoria para tests (guarda el último record
// guardado y el flujo precargado).
type fakeFlows struct {
	saved  *FlowRecord
	stored *FlowRecord
	err    error
}

func (f *fakeFlows) GetFlow(int64, string) (*FlowRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stored, nil
}

func (f *fakeFlows) SaveFlow(rec FlowRecord) error {
	if f.err != nil {
		return f.err
	}
	cp := rec
	f.saved = &cp
	return nil
}

func TestGetFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Usuario sin flujo: state vacío, nunca error (retomar es opcional).
	deps := &Deps{UserID: 7, Flows: &fakeFlows{}}
	out, err := GetFlow(ctx, deps, GetFlowIn{Agent: "cinema"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Agent != "cinema" || out.Step != 0 || len(out.Brief) != 0 || len(out.Artifacts) != 0 {
		t.Errorf("flujo nuevo = %+v", out)
	}

	// Flujo existente: se refleja el state guardado.
	stored := &FlowRecord{
		ID:        "flow-1",
		UserID:    7,
		Agent:     "cinema",
		ProjectID: "proj-1",
		State: map[string]any{
			"step":            float64(2),
			"conversation_id": "conv-9",
			"brief":           map[string]any{"guion": "escena A"},
			"artifacts":       map[string]any{"scene_analysis": map[string]any{"paso": float64(1)}},
		},
		UpdatedAt: time.Now(),
	}
	deps = &Deps{UserID: 7, Flows: &fakeFlows{stored: stored}}
	out, err = GetFlow(ctx, deps, GetFlowIn{Agent: "cinema"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Step != 2 || out.ProjectID != "proj-1" || out.ConversationID != "conv-9" {
		t.Errorf("out = %+v", out)
	}
	if out.Brief["guion"] != "escena A" {
		t.Errorf("brief = %v", out.Brief)
	}
	if _, ok := out.Artifacts["scene_analysis"]; !ok {
		t.Errorf("artifacts = %v", out.Artifacts)
	}

	// Agent obligatorio.
	if _, err := GetFlow(ctx, deps, GetFlowIn{}); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("sin agent err = %v", err)
	}
}

func TestSaveFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fake := &fakeFlows{}
	deps := &Deps{UserID: 7, Flows: fake}
	err := SaveFlow(ctx, deps, SaveFlowIn{Agent: "commercial", ProjectID: "p1", State: map[string]any{"step": float64(1)}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.saved == nil || fake.saved.UserID != 7 || fake.saved.Agent != "commercial" || fake.saved.ProjectID != "p1" {
		t.Fatalf("saved = %+v", fake.saved)
	}
	if fake.saved.ID == "" {
		t.Error("saved sin id generado")
	}
	if fake.saved.State["step"] != float64(1) {
		t.Errorf("state = %v", fake.saved.State)
	}

	// State nil se guarda como objeto vacío (JSONB NOT NULL DEFAULT '{}').
	if err := SaveFlow(ctx, deps, SaveFlowIn{Agent: "commercial"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.saved.State == nil {
		t.Error("state nil debería serializarse como {}")
	}

	if err := SaveFlow(ctx, deps, SaveFlowIn{}); err == nil {
		t.Error("sin agent debería fallar")
	}
}

func TestSaveArtifact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	artifact := map[string]any{"paso": float64(2), "prompt_hoja_storyboard": "hoja"}

	// Flujo nuevo: crea la fila con el artefacto y el brief del request.
	fake := &fakeFlows{}
	deps := &Deps{UserID: 7, Agent: "real_estate", Flows: fake}
	err := SaveArtifact(ctx, deps, SaveArtifactIn{
		Agent:          "real_estate",
		Step:           2,
		Kind:           "storyboard",
		Artifact:       artifact,
		ProjectID:      "proj-1",
		Brief:          map[string]any{"nombre_proyecto": "Torre A"},
		ConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.saved.State["step"] != float64(2) {
		t.Errorf("step = %v", fake.saved.State["step"])
	}
	if fake.saved.ProjectID != "proj-1" {
		t.Errorf("project_id = %q", fake.saved.ProjectID)
	}
	arts, _ := fake.saved.State["artifacts"].(map[string]any)
	if _, ok := arts["storyboard"]; !ok {
		t.Errorf("artifacts = %v", fake.saved.State["artifacts"])
	}

	// Flujo existente: mergea sin pisar el artefacto del paso anterior.
	fake = &fakeFlows{stored: &FlowRecord{
		ID:     "flow-1",
		UserID: 7,
		Agent:  "real_estate",
		State: map[string]any{
			"step": float64(1),
			"artifacts": map[string]any{
				"sales_angles": map[string]any{"angulos": []any{}},
			},
			"conversation_id": "conv-1",
		},
	}}
	deps = &Deps{UserID: 7, Flows: fake}
	err = SaveArtifact(ctx, deps, SaveArtifactIn{
		Agent:    "real_estate",
		Step:     2,
		Kind:     "storyboard",
		Artifact: artifact,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if fake.saved.ID != "flow-1" {
		t.Errorf("debería reutilizar la fila existente, id = %q", fake.saved.ID)
	}
	arts, _ = fake.saved.State["artifacts"].(map[string]any)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %v (quiero sales_angles + storyboard)", arts)
	}
	if _, ok := arts["sales_angles"]; !ok {
		t.Errorf("se pisó el artefacto previo: %v", arts)
	}
	// Sin brief en el request no se toca el existente.
	if _, ok := fake.saved.State["brief"]; ok {
		t.Errorf("brief tocado sin venir en el request: %v", fake.saved.State)
	}
	// Sin project_id en el request se conserva el previo ("" acá).
	if fake.saved.ProjectID != "" {
		t.Errorf("project_id = %q, quiero vacío", fake.saved.ProjectID)
	}

	// Validaciones.
	if err := SaveArtifact(ctx, deps, SaveArtifactIn{Agent: "real_estate"}); err == nil {
		t.Error("sin kind/artifact debería fallar")
	}
}

func TestMergeFlowState(t *testing.T) {
	t.Parallel()

	prev := map[string]any{
		"step":            float64(1),
		"conversation_id": "conv-1",
		"artifacts":       map[string]any{"sales_angles": map[string]any{"a": float64(1)}},
		"brief":           map[string]any{"x": "y"},
	}
	out := mergeFlowState(prev, SaveArtifactIn{
		Step:     3,
		Kind:     "clip_prompt",
		Artifact: map[string]any{"prompt_video": "0-8 s"},
		Brief:    map[string]any{"x": "z"},
	})

	if out["step"] != float64(3) {
		t.Errorf("step = %v", out["step"])
	}
	if out["conversation_id"] != "conv-1" {
		t.Errorf("conversation_id = %v", out["conversation_id"])
	}
	arts := out["artifacts"].(map[string]any)
	if len(arts) != 2 {
		t.Errorf("artifacts = %v", arts)
	}
	if out["brief"].(map[string]any)["x"] != "z" {
		t.Errorf("brief = %v", out["brief"])
	}
	// El previo no se muta (merge copy-on-write).
	if len(prev["artifacts"].(map[string]any)) != 1 {
		t.Errorf("prev mutado: %v", prev)
	}
}

func TestGetProjectBrief(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Sin agente en los deps no tiene sentido (fuera de un flujo).
	if _, err := GetProjectBrief(ctx, &Deps{UserID: 7}); err == nil {
		t.Fatal("sin agent debería fallar")
	}

	stored := &FlowRecord{
		ID:     "flow-1",
		UserID: 7,
		Agent:  "commercial",
		State: map[string]any{
			"step":      float64(1),
			"brief":     map[string]any{"marca": "Café Norte"},
			"artifacts": map[string]any{"scene_prompts": map[string]any{}},
		},
	}
	deps := &Deps{UserID: 7, Agent: "commercial", Flows: &fakeFlows{stored: stored}}
	out, err := GetProjectBrief(ctx, deps)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Agent != "commercial" || out.Step != 1 {
		t.Errorf("out = %+v", out)
	}
	if out.Brief["marca"] != "Café Norte" {
		t.Errorf("brief = %v", out.Brief)
	}
	if _, ok := out.Artifacts["scene_prompts"]; !ok {
		t.Errorf("artifacts = %v", out.Artifacts)
	}
	if out.Project != nil {
		t.Errorf("sin project_id no debería venir proyecto: %+v", out.Project)
	}
}
