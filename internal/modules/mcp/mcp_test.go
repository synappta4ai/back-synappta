package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mcpapi "github.com/modelcontextprotocol/go-sdk/mcp"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/event"
	"synapta/internal/toolkit"
)

// ─── Fakes del dominio ──────────────────────────────────────────

type fakeEvents struct {
	events    map[string]*event.Event
	order     []string
	pieces    map[string][]event.Piece // por eventID
	gens      map[string][]event.PieceGeneration
	createdEv []string
	createdPc []string
}

func newFakeEvents() *fakeEvents {
	return &fakeEvents{events: map[string]*event.Event{}, pieces: map[string][]event.Piece{}, gens: map[string][]event.PieceGeneration{}}
}

func (f *fakeEvents) CreateEvent(req *event.CreateEventRequest) (*event.Event, error) {
	ev := &event.Event{ID: "ev-" + req.Name, Name: req.Name, Venue: req.Venue, Description: req.Description, Status: "active", Active: true, CreatedAt: time.Now()}
	f.events[ev.ID] = ev
	f.order = append(f.order, ev.ID)
	f.createdEv = append(f.createdEv, req.Name)
	return ev, nil
}

func (f *fakeEvents) ListEvents(activeOnly bool) ([]event.Event, error) {
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
	if ev, ok := f.events[id]; ok {
		return ev, nil
	}
	return nil, nil // el store real devuelve nil,nil cuando no hay filas
}

func (f *fakeEvents) ListPieces(eventID, _ string) ([]event.Piece, error) {
	return f.pieces[eventID], nil
}

func (f *fakeEvents) CreatePiece(eventID string, req *event.CreatePieceRequest) (*event.Piece, error) {
	pc := event.Piece{ID: "pc-" + req.PieceCode, EventID: eventID, Number: req.Number, PieceCode: req.PieceCode, Name: req.Name, Type: req.Type, Active: true, CreatedAt: time.Now()}
	f.pieces[eventID] = append(f.pieces[eventID], pc)
	f.createdPc = append(f.createdPc, req.PieceCode)
	return &pc, nil
}

func (f *fakeEvents) GetPiece(id string) (*event.Piece, error) {
	for _, pcs := range f.pieces {
		for i := range pcs {
			if pcs[i].ID == id {
				return &pcs[i], nil
			}
		}
	}
	return nil, nil
}

func (f *fakeEvents) UpdateEvent(id string, req *event.UpdateEventRequest) (*event.Event, error) {
	ev, ok := f.events[id]
	if !ok {
		return nil, fmt.Errorf("event %q not found", id)
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

func (f *fakeEvents) ListGenerations(pieceID string) ([]event.PieceGeneration, error) {
	return f.gens[pieceID], nil
}

type fakeAgency struct {
	lastReq   *agency.GenerateRequest
	hist      []agency.GenerationLog
	status    *agency.StatusResponse
	statusID  string
	cancelled []string
	// sink registra la toma completada en la pieza, como el GenerationSaver
	// real; los tests de anclaje lo conectan al fakeEvents correspondiente.
	sink func(pieceID string, number int)
}

func (f *fakeAgency) GenerateUnified(req *agency.GenerateRequest) (*agency.GenerateResponse, error) {
	f.lastReq = req
	// Simula el sink real (SaveGenerationOutput): al completar, la toma queda
	// registrada en la pieza con su número de slot.
	if f.sink != nil {
		f.sink(req.PieceID, req.GenerationNumber)
	}
	return &agency.GenerateResponse{TaskID: "task-1", Status: "running", Model: req.Model}, nil
}

func (f *fakeAgency) UserHistoryForUser(_ int64, _ agency.UserHistoryFilter) ([]agency.GenerationLog, error) {
	return f.hist, nil
}

func (f *fakeAgency) GetStatusUnified(taskID string) (*agency.StatusResponse, error) {
	f.statusID = taskID
	if f.status == nil {
		return &agency.StatusResponse{Status: "running", ProgressPercent: 40}, nil
	}
	return f.status, nil
}

func (f *fakeAgency) CancelTask(taskID string) error {
	f.cancelled = append(f.cancelled, taskID)
	return nil
}

// ─── Helper de contexto ─────────────────────────────────────────

func ctxWith(deps *Deps) context.Context {
	return toolkit.WithDeps(context.Background(), deps)
}

// ─── Tools: create_project / list_projects ──────────────────────

func TestCreateProject(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	fe := newFakeEvents()
	deps := &Deps{Events: fe, Core: &fakeAgency{}, UserID: 7, Username: "agente"}

	_, out, err := m.createProject(ctxWith(deps), nil, createProjectIn{Name: "  Casamiento Lopez  ", Venue: "Salón Azul"})
	if err != nil {
		t.Fatalf("createProject: %v", err)
	}
	if out.ID == "" || out.Name != "Casamiento Lopez" || out.Venue != "Salón Azul" {
		t.Fatalf("salida inesperada: %+v", out)
	}
	if len(fe.createdEv) != 1 || fe.createdEv[0] != "Casamiento Lopez" {
		t.Fatalf("el evento no se creó con el nombre saneado: %v", fe.createdEv)
	}

	if _, _, err := m.createProject(ctxWith(deps), nil, createProjectIn{Name: "   "}); err == nil {
		t.Fatal("esperaba error por name vacío")
	}
}

func TestCreateProjectWithoutDepsFails(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	if _, _, err := m.createProject(context.Background(), nil, createProjectIn{Name: "X"}); err == nil || !strings.Contains(err.Error(), "tenant context missing") {
		t.Fatalf("esperaba 'tenant context missing', got %v", err)
	}
}

func TestListProjects(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	fe := newFakeEvents()
	_, _ = fe.CreateEvent(&event.CreateEventRequest{Name: "Activo"})
	inactive := &event.Event{ID: "ev-off", Name: "Archivado", Active: false}
	fe.events[inactive.ID] = inactive
	fe.order = append(fe.order, inactive.ID)
	deps := &Deps{Events: fe, Core: &fakeAgency{}, UserID: 1}

	_, active, err := m.listProjects(ctxWith(deps), nil, listProjectsIn{})
	if err != nil {
		t.Fatalf("listProjects: %v", err)
	}
	if len(active) != 1 || active[0].Name != "Activo" {
		t.Fatalf("default debe listar solo activos: %+v", active)
	}

	_, all, err := m.listProjects(ctxWith(deps), nil, listProjectsIn{All: true})
	if err != nil {
		t.Fatalf("listProjects(all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all=true debe listar los 2: %+v", all)
	}
}

// ─── generate_video ─────────────────────────────────────────────

func TestGenerateVideoAnchorsProjectAndPiece(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	fe := newFakeEvents()
	fa := &fakeAgency{sink: func(pieceID string, number int) {
		fe.gens[pieceID] = append(fe.gens[pieceID], event.PieceGeneration{PieceID: pieceID, Number: number})
	}}
	deps := &Deps{Events: fe, Core: fa, UserID: 7, Username: "agente"}

	// Sin project_id: find-or-create por nombre; sin pieza: find-or-create por código.
	_, out, err := m.generateVideo(ctxWith(deps), nil, generateVideoIn{
		ProjectName: "Agencia Test", Prompt: "playa al atardecer", Model: "higgsfield-seedance-2-5-t2v",
	})
	if err != nil {
		t.Fatalf("generateVideo: %v", err)
	}
	if out.EventName != "Agencia Test" || out.PieceCode != "MCP" || out.GenerationNumber != 1 {
		t.Fatalf("anclaje inesperado: %+v", out)
	}
	if fa.lastReq == nil {
		t.Fatal("no se invocó GenerateUnified")
	}
	if fa.lastReq.UserID != 7 || fa.lastReq.UserName != "agente" || fa.lastReq.ResourceType != "video" {
		t.Fatalf("caller no inyectado: %+v", fa.lastReq)
	}
	if fa.lastReq.EventID != out.EventID || fa.lastReq.PieceID != out.PieceID || fa.lastReq.PieceCode != out.PieceCode {
		t.Fatalf("request desalineado con la salida: %+v", fa.lastReq)
	}
	if fa.lastReq.Duration != 5 {
		t.Fatalf("duration debe defaultear a 5, got %v", fa.lastReq.Duration)
	}
	if len(fa.lastReq.Content) != 1 || fa.lastReq.Content[0].Type != "text" || fa.lastReq.Content[0].Text != "playa al atardecer" {
		t.Fatalf("content inesperado: %+v", fa.lastReq.Content)
	}

	// Segunda llamada al mismo proyecto reutiliza pieza y sube el número de slot.
	_, out2, err := m.generateVideo(ctxWith(deps), nil, generateVideoIn{
		ProjectName: "Agencia Test", Prompt: "otra toma", Model: "m", ReferenceImageIDs: []string{"file-9", " "},
	})
	if err != nil {
		t.Fatalf("generateVideo 2: %v", err)
	}
	if out2.EventID != out.EventID || out2.PieceID != out.PieceID {
		t.Fatalf("debía reutilizar proyecto y pieza: %+v", out2)
	}
	if out2.GenerationNumber != 2 {
		t.Fatalf("generation_number debe ser 2, got %d", out2.GenerationNumber)
	}
	if len(fa.lastReq.Content) != 2 || fa.lastReq.Content[1].ID != "file-9" {
		t.Fatalf("imagen de referencia no adjunta: %+v", fa.lastReq.Content)
	}
	if len(fe.createdEv) != 1 {
		t.Fatalf("no debía crear un segundo proyecto: %v", fe.createdEv)
	}
}

func TestGenerateVideoValidation(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	deps := &Deps{Events: newFakeEvents(), Core: &fakeAgency{}, UserID: 1}

	cases := []struct {
		name string
		in   generateVideoIn
		want string
	}{
		{"prompt vacío", generateVideoIn{Model: "m"}, "prompt is required"},
		{"model vacío", generateVideoIn{Prompt: "p"}, "model is required"},
		{"proyecto inexistente", generateVideoIn{ProjectID: "no-existe", Prompt: "p", Model: "m"}, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := m.generateVideo(ctxWith(deps), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("esperaba %q, got %v", tc.want, err)
			}
		})
	}
}

func TestGenerateVideoPieceFromAnotherProjectRejected(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	fe := newFakeEvents()
	_, _ = fe.CreateEvent(&event.CreateEventRequest{Name: "A"})
	_, _ = fe.CreateEvent(&event.CreateEventRequest{Name: "B"})
	_, _ = fe.CreatePiece("ev-B", &event.CreatePieceRequest{Number: 1, PieceCode: "PC"})
	deps := &Deps{Events: fe, Core: &fakeAgency{}, UserID: 1}

	_, _, err := m.generateVideo(ctxWith(deps), nil, generateVideoIn{
		ProjectID: "ev-A", PieceID: "pc-PC", Prompt: "p", Model: "m",
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("esperaba rechazo de pieza ajena, got %v", err)
	}
}

func TestNextGenerationNumberSkipsGaps(t *testing.T) {
	t.Parallel()
	fe := newFakeEvents()
	fe.gens["pc-1"] = []event.PieceGeneration{{Number: 1}, {Number: 4}, {Number: 2}}
	n, err := toolkit.NextGenerationNumber(fe, "pc-1")
	if err != nil {
		t.Fatalf("NextGenerationNumber: %v", err)
	}
	if n != 5 {
		t.Fatalf("esperaba max+1=5, got %d", n)
	}
}

// ─── list_generations ───────────────────────────────────────────

func TestListGenerationsPollsTaskByID(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	fa := &fakeAgency{status: &agency.StatusResponse{Status: "completed", ProgressPercent: 100, Outputs: []agency.OutputResource{{URL: "http://v/1.mp4", Type: "video"}}}}
	deps := &Deps{Events: newFakeEvents(), Core: fa, UserID: 1}

	_, out, err := m.listGenerations(ctxWith(deps), nil, listGenerationsIn{TaskID: "task-9"})
	if err != nil {
		t.Fatalf("listGenerations: %v", err)
	}
	if fa.statusID != "task-9" {
		t.Fatalf("no consultó el task pedido: %q", fa.statusID)
	}
	if len(out) != 1 || out[0].Status != "completed" || out[0].ProgressPercent != 100 || len(out[0].Outputs) != 1 {
		t.Fatalf("poll inesperado: %+v", out)
	}
}

func TestListGenerationsMapsHistory(t *testing.T) {
	t.Parallel()
	m := NewModule(nil, nil)
	now := time.Now()
	fa := &fakeAgency{hist: []agency.GenerationLog{{
		TaskID: "t-1", ModelName: "seedance", Status: "success", EventID: "ev-1",
		PieceCode: "MCP", GenerationNumber: 3, ResourceType: "video", Progress: 100, CreatedAt: now,
	}}}
	deps := &Deps{Events: newFakeEvents(), Core: fa, UserID: 42}

	_, out, err := m.listGenerations(ctxWith(deps), nil, listGenerationsIn{Limit: 500})
	if err != nil {
		t.Fatalf("listGenerations: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("esperaba 1 fila, got %d", len(out))
	}
	g := out[0]
	if g.TaskID != "t-1" || g.Model != "seedance" || g.GenerationNumber != 3 || g.CreatedAt == nil {
		t.Fatalf("mapeo inesperado: %+v", g)
	}
}

// ─── Transporte end-to-end (cliente oficial del SDK) ────────────

// testRouter monta el módulo real sobre gin con deps fakes, igual que el
// bridge de producción pero sin base de datos.
func testRouter(t *testing.T, deps *Deps, authed bool) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	m := NewModule(nil, nil)
	m.depsFor = func(c *gin.Context) (*Deps, bool) {
		if !authed {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return nil, false
		}
		return deps, true
	}
	r := gin.New()
	v1 := r.Group("/api/v1")
	// authMw/tenantMw simulados: la cadena real ya probó su parte en main.
	m.Register(v1, func(c *gin.Context) { c.Next() }, func(c *gin.Context) { c.Next() }, nil)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// newClient conecta el cliente oficial del SDK (stateless, sin SSE). Los
// headers (p.ej. Authorization) se inyectan vía RoundTripper, ya que el
// transporte del SDK no expone un mapa de headers.
func newClient(t *testing.T, endpoint string, headers map[string]string) *mcpapi.ClientSession {
	t.Helper()
	cli := mcpapi.NewClient(&mcpapi.Implementation{Name: "test-client", Version: "0"}, nil)
	tr := &headerRoundTripper{base: http.DefaultTransport, headers: headers}
	session, err := cli.Connect(context.Background(), &mcpapi.StreamableClientTransport{
		Endpoint:             endpoint + "/api/v1/mcp",
		HTTPClient:           &http.Client{Transport: tr},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// headerRoundTripper agrega headers fijos a cada request del cliente MCP.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

func TestTransportInitializeAndToolsList(t *testing.T) {
	t.Parallel()
	deps := &Deps{Events: newFakeEvents(), Core: &fakeAgency{}, UserID: 1, Username: "agente"}
	srv := testRouter(t, deps, true)
	session := newClient(t, srv.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"create_project", "list_projects", "list_models", "generate_video", "list_generations"} {
		if !names[want] {
			t.Fatalf("falta la tool %q en tools/list: %v", want, names)
		}
	}
	// El input schema debe marcar prompt y model como requeridos en generate_video.
	for _, tool := range res.Tools {
		if tool.Name != "generate_video" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("unmarshal schema: %v", err)
		}
		req := map[string]bool{}
		for _, k := range schema.Required {
			req[k] = true
		}
		if !req["prompt"] || !req["model"] {
			t.Fatalf("generate_video debe requerir prompt y model, required=%v", schema.Required)
		}
	}
}

func TestTransportToolsCallCreateProject(t *testing.T) {
	t.Parallel()
	fe := newFakeEvents()
	deps := &Deps{Events: fe, Core: &fakeAgency{}, UserID: 7, Username: "agente"}
	srv := testRouter(t, deps, true)
	session := newClient(t, srv.URL, nil)

	res, err := session.CallTool(context.Background(), &mcpapi.CallToolParams{
		Name:      "create_project",
		Arguments: map[string]any{"name": "Feria Tech", "venue": "Centro"},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	if len(fe.createdEv) != 1 || fe.createdEv[0] != "Feria Tech" {
		t.Fatalf("el side-effect no llegó al dominio: %v", fe.createdEv)
	}
	// El structured output debe traer el id creado.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var out projectOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.ID == "" || out.Name != "Feria Tech" {
		t.Fatalf("structured output inesperado: %+v", out)
	}
}

func TestTransportToolsCallGenerateVideo(t *testing.T) {
	t.Parallel()
	fe := newFakeEvents()
	fa := &fakeAgency{}
	deps := &Deps{Events: fe, Core: fa, UserID: 7, Username: "agente"}
	srv := testRouter(t, deps, true)
	session := newClient(t, srv.URL, nil)

	res, err := session.CallTool(context.Background(), &mcpapi.CallToolParams{
		Name: "generate_video",
		Arguments: map[string]any{
			"project_name": "MCP E2E",
			"prompt":       "un dron sobre la ciudad",
			"model":        "higgsfield-seedance-2-5-t2v",
		},
	})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
	if fa.lastReq == nil || fa.lastReq.EventID == "" || fa.lastReq.PieceCode != "MCP" {
		t.Fatalf("la generación no se ancló: %+v", fa.lastReq)
	}
	if fa.lastReq.UserID != 7 || fa.lastReq.UserName != "agente" {
		t.Fatalf("caller del ctx no viajó al core: %+v", fa.lastReq)
	}
}

func TestTransportUnauthenticatedRejected(t *testing.T) {
	t.Parallel()
	deps := &Deps{Events: newFakeEvents(), Core: &fakeAgency{}, UserID: 1}
	srv := testRouter(t, deps, false)

	// Sin el client (que abortaría en Connect), pegamos directo al endpoint.
	resp, err := http.Post(srv.URL+"/api/v1/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("esperaba 401 sin auth, got %d", resp.StatusCode)
	}
}
