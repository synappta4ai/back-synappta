package writingcopilot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"synapta/internal/modules/credential"
)

func TestLoadWritingCopilotSystemPrompt(t *testing.T) {
	body, err := LoadWritingCopilotSystemPrompt()
	if err != nil {
		t.Fatalf("LoadWritingCopilotSystemPrompt() error = %v", err)
	}
	if strings.HasPrefix(body, "---") {
		t.Error("el system prompt todavia incluye el frontmatter")
	}
	if !strings.Contains(body, "Writing Copilot") {
		t.Error("el system prompt no parece el skill de Writing Copilot")
	}
	name, _, perr := parseSkill(skillRaw)
	if perr != nil {
		t.Fatalf("parseSkill() error = %v", perr)
	}
	if name != skillName {
		t.Errorf("name = %q, se esperaba %q", name, skillName)
	}
}

func TestParseSkillNormalizesCRLFAndBOM(t *testing.T) {
	raw := "\ufeff---\r\nname: Synappta-WRITE\r\ndescription: test\r\n---\r\n\r\ncuerpo\r\n"
	name, body, err := parseSkill(raw)
	if err != nil {
		t.Fatalf("parseSkill() error = %v", err)
	}
	if name != skillName {
		t.Errorf("name = %q", name)
	}
	if body != "cuerpo" {
		t.Errorf("body = %q", body)
	}
}

func TestLoadSkillRejectsWrongName(t *testing.T) {
	if _, err := loadSkill("---\nname: Otra-Cosa\n---\ncuerpo\n"); err == nil {
		t.Fatal("se esperaba error con un name distinto de Synappta-WRITE")
	}
	if _, err := loadSkill(""); err == nil {
		t.Fatal("se esperaba error con un skill vacio")
	}
}

func TestEnabledFlag(t *testing.T) {
	t.Setenv(envEnabled, "true")
	if !Enabled() {
		t.Error("el agente debe estar habilitado por defecto")
	}
	t.Setenv(envEnabled, "false")
	if Enabled() {
		t.Error("WRITING_COPILOT_ENABLED=false debe deshabilitar el agente")
	}
}

func TestSplitModelAndTemperature(t *testing.T) {
	t.Setenv(envModel, "")
	if provider, model := splitModel(""); provider != credential.ProviderOpenRouter || model != defaultModel {
		t.Errorf("sin modelo: provider=%q model=%q", provider, model)
	}
	if provider, model := splitModel("openrouter/anthropic/claude-haiku-5.5"); provider != credential.ProviderOpenRouter || model != "anthropic/claude-haiku-5.5" {
		t.Errorf("openrouter: provider=%q model=%q", provider, model)
	}
	if provider, model := splitModel("anthropic/claude-haiku-4-5"); provider != credential.ProviderAnthropic || model != "claude-haiku-4-5" {
		t.Errorf("anthropic directo: provider=%q model=%q", provider, model)
	}
	t.Setenv(envModel, "anthropic/claude-sonnet-5.5")
	if provider, model := splitModel("openrouter/google/gemini-3.8-flash"); provider != credential.ProviderAnthropic || model != "claude-sonnet-5.5" {
		t.Errorf("la env debe ganar: provider=%q model=%q", provider, model)
	}
	t.Setenv(envModel, "")
	t.Setenv(envTemperature, "")
	if got := copilotTemperature(); got != defaultTemperature {
		t.Errorf("temperatura por defecto = %v", got)
	}
	t.Setenv(envTemperature, "1.1")
	if got := copilotTemperature(); got != 1.1 {
		t.Errorf("temperatura = %v", got)
	}
	t.Setenv(envTemperature, "9")
	if got := copilotTemperature(); got != defaultTemperature {
		t.Errorf("temperatura fuera de rango debe caer al default, obtuvo %v", got)
	}
}

func TestBuildUserMessageOrdersParamsReferencesAndMessage(t *testing.T) {
	total := 30.0
	shots := 4
	req := chatRequest{
		Message:    "una persona camina de noche",
		References: "cliente: joyeria",
		Params: &chatParams{
			OutputLanguage: "es",
			Delivery:       "shot_pack",
			AspectRatio:    "9:16",
			TotalDurationS: &total,
			AudioMode:      "with_dialogue",
			ShotCount:      &shots,
			ResponseMode:   "prompt_only",
		},
		History: []historyMessage{
			{Role: "user", Content: "hola"},
			{Role: "assistant", Content: "hola, contame la idea"},
			{Role: "system", Content: "esto se descarta"},
			{Role: "user", Content: "  "},
		},
	}
	messages := buildUserMessage(req, nil)
	if len(messages) != 3 {
		t.Fatalf("mensajes = %d, se esperaban 3 (2 de historial + el actual)", len(messages))
	}
	if messages[0]["role"] != "user" || messages[1]["role"] != "assistant" {
		t.Errorf("el historial debe conservar el orden: %v", messages)
	}
	current, _ := messages[2]["content"].(string)
	for _, want := range []string{"PARAMS:", "output_language: es", "delivery: shot_pack", "aspect_ratio: 9:16", "total_duration_s: 30", "audio_mode: with_dialogue", "shot_count: 4", "response_mode: prompt_only", "REFERENCES:", "cliente: joyeria", "una persona camina de noche"} {
		if !strings.Contains(current, want) {
			t.Errorf("falta %q en el mensaje", want)
		}
	}
	if strings.Contains(current, "max_clip_s") || strings.Contains(current, "fps") || strings.Contains(current, "look") || strings.Contains(current, "target_model") {
		t.Errorf("solo deben aparecer los parametros informados: %s", current)
	}
	if strings.Index(current, "PARAMS:") > strings.Index(current, "REFERENCES:") {
		t.Error("PARAMS debe ir antes de REFERENCES")
	}
	if strings.Index(current, "REFERENCES:") > strings.Index(current, "una persona camina de noche") {
		t.Error("REFERENCES debe ir antes del mensaje")
	}
}

func TestBuildUserMessageWithoutParamsOmitsBlock(t *testing.T) {
	messages := buildUserMessage(chatRequest{Message: "idea"}, nil)
	if len(messages) != 1 {
		t.Fatalf("mensajes = %d", len(messages))
	}
	content := messages[0]["content"].(string)
	if strings.Contains(content, "PARAMS:") || strings.Contains(content, "REFERENCES:") {
		t.Errorf("sin parametros no debe haber bloques vacios: %s", content)
	}
	if content != "idea" {
		t.Errorf("contenido = %q", content)
	}
}

func TestBuildUserMessageCapsHistory(t *testing.T) {
	history := make([]historyMessage, maxHistoryMessages+5)
	for i := range history {
		history[i] = historyMessage{Role: "user", Content: "turno"}
	}
	messages := buildUserMessage(chatRequest{Message: "final", History: history}, nil)
	if len(messages) != maxHistoryMessages+1 {
		t.Fatalf("mensajes = %d, se esperaba %d", len(messages), maxHistoryMessages+1)
	}
}

func TestExtractPrompts(t *testing.T) {
	text := "Intro\n\n```\nPROMPT UNO\ncon dos lineas\n```\n\nmedio\n\n```markdown\nPROMPT DOS\n```\n\n```\n\n```\nfin"
	prompts := extractPrompts(text)
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d, se esperaban 2 (el bloque vacio se ignora): %v", len(prompts), prompts)
	}
	if prompts[0] != "PROMPT UNO\ncon dos lineas" {
		t.Errorf("prompt 0 = %q", prompts[0])
	}
	if prompts[1] != "PROMPT DOS" {
		t.Errorf("prompt 1 = %q", prompts[1])
	}
	if got := extractPrompts("sin bloques"); len(got) != 0 {
		t.Errorf("sin bloques debe devolver vacio: %v", got)
	}
}

func TestModuleNameAndAvailabilityRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := NewModule(nil, nil)
	if m.Name() != "writing_copilot" {
		t.Errorf("Name() = %q", m.Name())
	}
	router := gin.New()
	noop := func(*gin.Context) {}
	group := router.Group("/api/v1")
	m.Register(group, noop, noop, noop)

	t.Setenv(envEnabled, "true")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/writing-copilot/availability", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("availability status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("availability no es JSON: %v", err)
	}
	data, _ := body["data"].(map[string]any)
	if data["enabled"] != true {
		t.Errorf("enabled = %v", data["enabled"])
	}

	t.Setenv(envEnabled, "false")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/writing-copilot/availability", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	data, _ = body["data"].(map[string]any)
	if data["enabled"] != false {
		t.Errorf("enabled con flag off = %v", data["enabled"])
	}

	// El chat responde un error controlado cuando el agente esta deshabilitado.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/writing-copilot/chat", strings.NewReader(`{"message":"hola"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("chat con agente off = %d, se esperaba 503", rec.Code)
	}
}

func TestChatRejectsEmptyMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv(envEnabled, "true")
	m := NewModule(nil, nil)
	router := gin.New()
	noop := func(*gin.Context) {}
	m.Register(router.Group("/api/v1"), noop, noop, noop)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/writing-copilot/chat", strings.NewReader(`{"message":"   "}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mensaje vacio = %d, se esperaba 400", rec.Code)
	}
}

func TestValidVideoType(t *testing.T) {
	for _, vt := range []string{"", VideoTypeCommercialShort, VideoTypeCommercialLong, VideoTypeStoryShort, VideoTypeStoryLong} {
		if !ValidVideoType(vt) {
			t.Errorf("ValidVideoType(%q) = false", vt)
		}
	}
	for _, vt := range []string{"otro", "commercial", "short"} {
		if ValidVideoType(vt) {
			t.Errorf("ValidVideoType(%q) = true", vt)
		}
	}
}

func TestSystemPromptForVideoType(t *testing.T) {
	base, err := LoadWritingCopilotSystemPrompt()
	if err != nil {
		t.Fatalf("LoadWritingCopilotSystemPrompt() error = %v", err)
	}
	cases := []struct {
		videoType string
		marker    string
	}{
		{VideoTypeCommercialShort, "MOTOR DE STORYTELLING COMERCIAL"},
		{VideoTypeCommercialLong, "MOTOR DE STORYTELLING COMERCIAL"},
		{VideoTypeStoryShort, "MOTOR DE REESTRUCTURACIÓN Y ENRIQUECIMIENTO NARRATIVO"},
		{VideoTypeStoryLong, "NARRATIVE CORE (long story)"},
	}
	for _, tc := range cases {
		prompt, err := SystemPromptForVideoType(tc.videoType)
		if err != nil {
			t.Fatalf("SystemPromptForVideoType(%q) error = %v", tc.videoType, err)
		}
		if !strings.HasPrefix(prompt, base) {
			t.Errorf("%q: el prompt debe empezar con el skill base", tc.videoType)
		}
		if !strings.Contains(prompt, "STEP 1 STRUCTURE GUIDE") {
			t.Errorf("%q: falta la seccion de estructura", tc.videoType)
		}
		if !strings.Contains(prompt, tc.marker) {
			t.Errorf("%q: falta el marcador %q", tc.videoType, tc.marker)
		}
	}
	for _, vt := range []string{"", "otro"} {
		prompt, err := SystemPromptForVideoType(vt)
		if err != nil {
			t.Fatalf("SystemPromptForVideoType(%q) error = %v", vt, err)
		}
		if !strings.HasPrefix(prompt, base) {
			t.Errorf("%q: el prompt debe empezar con el skill base", vt)
		}
		if strings.Contains(prompt, "STEP 1 STRUCTURE GUIDE") {
			t.Errorf("%q: sin tipo valido no debe haber guia de estructura", vt)
		}
		if !strings.Contains(prompt, "ACTING ENGINE") {
			t.Errorf("%q: falta el motor de actuacion", vt)
		}
	}
}

func TestRenderParamsIncludesVideoType(t *testing.T) {
	got := renderParams(&chatParams{VideoType: VideoTypeStoryLong})
	if !strings.Contains(got, "video_type: story_long") {
		t.Errorf("renderParams() = %q", got)
	}
}

func TestEmbeddedGuidesMatchPromptOriginals(t *testing.T) {
	cases := []struct {
		file  string
		guide string
	}{
		{"master-prompt-publicidad-adaptable.md", commercialGuide},
		{"master-prompt-guion-cinematografico.md", narrativeGuide},
	}
	for _, tc := range cases {
		raw, err := os.ReadFile(filepath.Join("..", "..", "Prompt", tc.file))
		if err != nil {
			t.Fatalf("no se pudo leer internal/Prompt/%s: %v", tc.file, err)
		}
		if string(raw) != tc.guide {
			t.Errorf("internal/Prompt/%s y prompts/%s estan desincronizados", tc.file, tc.file)
		}
	}
}

func TestSystemPromptIncludesActingEngine(t *testing.T) {
	for _, videoType := range []string{"", VideoTypeCommercialShort, VideoTypeStoryLong} {
		prompt, err := SystemPromptForVideoType(videoType)
		if err != nil {
			t.Fatalf("SystemPromptForVideoType(%q) error = %v", videoType, err)
		}
		if !strings.Contains(prompt, "ACTING ENGINE") || !strings.Contains(prompt, "Weston step") {
			t.Errorf("%q: el prompt no incluye el motor de actuacion", videoType)
		}
	}
}
