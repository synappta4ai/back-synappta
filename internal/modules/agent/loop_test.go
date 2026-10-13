package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/event"
	"synapta/internal/toolkit"
)

// ─── Fakes del dominio ──────────────────────────────────────────

type fakeEvents struct {
	mu      sync.Mutex
	events  map[string]*event.Event
	order   []string
	created []string
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{events: map[string]*event.Event{}}
}

func (f *fakeEvents) CreateEvent(req *event.CreateEventRequest) (*event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev := &event.Event{ID: "ev-" + req.Name, Name: req.Name, Venue: req.Venue, Description: req.Description, Active: true, CreatedAt: time.Now()}
	f.events[ev.ID] = ev
	f.order = append(f.order, ev.ID)
	f.created = append(f.created, req.Name)
	return ev, nil
}

func (f *fakeEvents) ListEvents(activeOnly bool) ([]event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]event.Event, 0, len(f.order))
	for _, id := range f.order {
		ev := f.events[id]
		if activeOnly && !ev.Active {
			continue
		}
		out = append(out, *ev)
	}
	return out, nil
}

func (f *fakeEvents) GetEvent(id string) (*event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev, ok := f.events[id]; ok {
		return ev, nil
	}
	return nil, nil
}

func (f *fakeEvents) UpdateEvent(id string, req *event.UpdateEventRequest) (*event.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ev, ok := f.events[id]
	if !ok {
		return nil, nil
	}
	if req.Name != nil {
		ev.Name = *req.Name
	}
	if req.Venue != nil {
		ev.Venue = *req.Venue
	}
	if req.Description != nil {
		ev.Description = *req.Description
	}
	if req.Status != nil {
		ev.Status = *req.Status
	}
	return ev, nil
}

func (f *fakeEvents) ListPieces(eventID, _ string) ([]event.Piece, error) {
	return nil, nil
}

func (f *fakeEvents) CreatePiece(eventID string, req *event.CreatePieceRequest) (*event.Piece, error) {
	pc := event.Piece{ID: "pc-" + req.PieceCode, EventID: eventID, Number: req.Number, PieceCode: req.PieceCode, Name: req.Name, Type: req.Type, Active: true, CreatedAt: time.Now()}
	return &pc, nil
}

func (f *fakeEvents) GetPiece(id string) (*event.Piece, error) {
	return nil, nil
}

func (f *fakeEvents) ListGenerations(pieceID string) ([]event.PieceGeneration, error) {
	return nil, nil
}

type fakeAgency struct {
	mu        sync.Mutex
	lastReq   *agency.GenerateRequest
	cancelled []string
}

func (f *fakeAgency) GenerateUnified(req *agency.GenerateRequest) (*agency.GenerateResponse, error) {
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	return &agency.GenerateResponse{TaskID: "task-42", Status: "running", Model: req.Model}, nil
}

func (f *fakeAgency) UserHistoryForUser(_ int64, _ agency.UserHistoryFilter) ([]agency.GenerationLog, error) {
	return []agency.GenerationLog{{
		TaskID: "task-42", ModelName: "vid", Status: "running", Progress: 42,
		EventID: "ev-X", PieceCode: "PC", GenerationNumber: 1, ResourceType: "video",
		CreatedAt: time.Now(),
	}}, nil
}

func (f *fakeAgency) GetStatusUnified(taskID string) (*agency.StatusResponse, error) {
	return &agency.StatusResponse{Status: "running", ProgressPercent: 77}, nil
}

func (f *fakeAgency) CancelTask(taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, taskID)
	return nil
}

func testDeps() (*toolkit.Deps, *fakeEvents, *fakeAgency) {
	fe, fa := newFakeEvents(), &fakeAgency{}
	return &toolkit.Deps{Events: fe, Core: fa, UserID: 7, Username: "ana"}, fe, fa
}

// ─── Loop con provider OpenAI (OpenRouter) ──────────────────────

// TestLoopOpenAICallsToolThenAnswers emula el provider: el primer request
// pide un tool call, el segundo devuelve la respuesta final.
func TestLoopOpenAICallsToolThenAnswers(t *testing.T) {
	t.Parallel()

	deps, fe, _ := testDeps()
	_ = deps
	var steps int
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastBody = body
		if steps == 1 {
			// Pide create_project.
			w.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"create_project","arguments":"{\"name\":\"Boda Lopez\"}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"Listo, creé el proyecto Boda Lopez."}}]}`))
	}))
	defer srv.Close()

	cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-test"}
	var events []toolEvent
	text, err := runToolLoop(toolkit.WithDeps(context.Background(), deps), srv.Client(), credential.ProviderOpenRouter, cred,
		"openai/gpt-4o-mini", "sys", []chatMsg{{Role: "user", Content: "creá el proyecto Boda Lopez"}},
		agentTools(), chatMaxTokens, func(ev toolEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if text != "Listo, creé el proyecto Boda Lopez." {
		t.Errorf("text=%q", text)
	}
	if steps != 2 {
		t.Errorf("pasos=%d, want 2 (tool + respuesta)", steps)
	}
	// El tool creó el proyecto real (fake) del tenant.
	if len(fe.created) != 1 || fe.created[0] != "Boda Lopez" {
		t.Errorf("created=%v", fe.created)
	}
	// El segundo request al LLM debe traer el tool result con role "tool".
	msgs, _ := lastBody["messages"].([]any)
	found := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" && strings.Contains(mm["content"].(string), "ev-Boda Lopez") {
			found = true
		}
	}
	if !found {
		t.Errorf("el segundo request no incluyó el tool result: %v", msgs)
	}
	// El front recibió los eventos running+ok del tool.
	if len(events) != 2 || events[0].Status != "running" || events[1].Status != "ok" {
		t.Errorf("events=%+v", events)
	}
}

// TestLoopAnthropicToolUse ejercita el formato Anthropic (bloques tool_use /
// tool_result) con el mismo flujo de dos pasos.
func TestLoopAnthropicToolUse(t *testing.T) {
	t.Parallel()

	deps, _, fa := testDeps()
	_ = deps
	var steps int
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastBody = body
		if steps == 1 {
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"tu-1","name":"generate_video","input":{"prompt":"fachada al atardecer","model":"vid-model","project_name":"Altos"}}]}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Video encolado, task task-42."}]}`))
	}))
	defer srv.Close()

	cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-ant"}
	text, err := runToolLoop(toolkit.WithDeps(context.Background(), deps), srv.Client(), credential.ProviderAnthropic, cred,
		"claude-haiku-4-5", "sys", []chatMsg{{Role: "user", Content: "generá el video"}},
		agentTools(), chatMaxTokens, nil)
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if text != "Video encolado, task task-42." {
		t.Errorf("text=%q", text)
	}
	// El fake del core recibió la generación con el anclaje correcto.
	if fa.lastReq == nil || fa.lastReq.EventID == "" || fa.lastReq.PieceCode == "" || fa.lastReq.GenerationNumber != 1 {
		t.Errorf("lastReq=%+v", fa.lastReq)
	}
	// El segundo request trae el bloque tool_result con is_error=false.
	msgs, _ := lastBody["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	blocks, _ := last["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("sin tool_result: %v", msgs)
	}
	block, _ := blocks[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "tu-1" {
		t.Errorf("block=%v", block)
	}
}

// TestLoopToolErrorFlowsBackToLLM: un tool que falla no tira el turno; el
// error vuelve como tool result y el LLM responde igual.
func TestLoopToolErrorFlowsBackToLLM(t *testing.T) {
	t.Parallel()

	deps, _, _ := testDeps()
	var steps int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		if steps == 1 {
			w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call-1","type":"function","function":{"name":"list_generations","arguments":"{\"task_id\":\"no-existe\"}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"No pude consultar la tarea."}}]}`))
	}))
	defer srv.Close()

	// status unified del fake no falla; usamos un core que sí falla.
	deps.Core = &failingCore{}
	cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk-test"}
	text, err := runToolLoop(toolkit.WithDeps(context.Background(), deps), srv.Client(), credential.ProviderOpenRouter, cred,
		"m", "s", []chatMsg{{Role: "user", Content: "¿cómo va la tarea?"}}, agentTools(), chatMaxTokens, nil)
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if text != "No pude consultar la tarea." {
		t.Errorf("text=%q", text)
	}
}

// failingCore falla el status: simula un provider caído para el tool de poll.
type failingCore struct{ fakeAgency }

func (f *failingCore) GetStatusUnified(taskID string) (*agency.StatusResponse, error) {
	return nil, context.DeadlineExceeded
}

// TestLoopUnknownToolIsReported: un tool inexistente vuelve como error al
// LLM en vez de romper el turno.
func TestLoopUnknownToolIsReported(t *testing.T) {
	t.Parallel()

	_, _, _ = testDeps()
	var steps int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		steps++
		if steps == 1 {
			w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"delete_everything","arguments":"{}"}}]}}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"No tengo esa herramienta."}}]}`))
	}))
	defer srv.Close()

	deps, _, _ := testDeps()
	cred := &credential.Resolve{BaseURL: srv.URL, APIKey: "sk"}
	text, err := runToolLoop(toolkit.WithDeps(context.Background(), deps), srv.Client(),
		credential.ProviderOpenRouter, cred, "m", "s", []chatMsg{{Role: "user", Content: "borrá todo"}}, agentTools(), chatMaxTokens, nil)
	if err != nil {
		t.Fatalf("runToolLoop: %v", err)
	}
	if text != "No tengo esa herramienta." {
		t.Errorf("text=%q", text)
	}
}

// TestExecuteToolWithoutDepsFails: invocar un tool sin los servicios del
// tenant en el ctx devuelve error (nunca opera a ciegas).
func TestExecuteToolWithoutDepsFails(t *testing.T) {
	t.Parallel()

	result, ev, _ := executeTool(context.Background(), agentTools(), toolCall{Name: "list_projects", Args: nil}, nil)
	if !strings.Contains(result, "tenant context missing") {
		t.Errorf("result=%q", result)
	}
	if ev.Status != "error" {
		t.Errorf("ev=%+v", ev)
	}
}

// TestExecuteToolRuns: con deps en el ctx el tool corre y devuelve JSON.
func TestExecuteToolRuns(t *testing.T) {
	t.Parallel()

	deps, _, _ := testDeps()
	ctx := toolkit.WithDeps(context.Background(), deps)
	result, ev, _ := executeTool(ctx, agentTools(), toolCall{Name: "create_project", Args: json.RawMessage(`{"name":"Casamiento Ana"}`)}, nil)
	if ev.Status != "ok" {
		t.Errorf("ev=%+v", ev)
	}
	if !strings.Contains(result, "ev-Casamiento Ana") {
		t.Errorf("result=%q", result)
	}
}

// TestCancelTaskTool cancela vía el core del tenant.
func TestCancelTaskTool(t *testing.T) {
	t.Parallel()

	deps, _, fa := testDeps()
	ctx := toolkit.WithDeps(context.Background(), deps)
	result, ev, _ := executeTool(ctx, agentTools(), toolCall{Name: "cancel_task", Args: json.RawMessage(`{"task_id":"task-9"}`)}, nil)
	if ev.Status != "ok" {
		t.Errorf("ev=%+v", ev)
	}
	if !strings.Contains(result, "cancelled") {
		t.Errorf("result=%q", result)
	}
	if len(fa.cancelled) != 1 || fa.cancelled[0] != "task-9" {
		t.Errorf("cancelled=%v", fa.cancelled)
	}
}
