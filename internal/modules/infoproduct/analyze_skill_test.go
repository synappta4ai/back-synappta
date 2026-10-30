package infoproduct

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleAnalyzeJSON = `{
  "schema_version": "dcs-analyze/1.0",
  "video": {"duration_s": 3.0, "aspect_ratio": "9:16", "resolution": "1080x1920", "fps": 30, "shot_count": 2, "format": "ugc"},
  "legacy_ui": {
    "categoria": "ugc", "duracion_s": 3.0, "relacion_aspecto": "9:16", "numero_de_tomas": 2,
    "resolucion_fps": "1080x1920 · 30", "resumen": "Dos cortes rapidos sobre un anillo.",
    "ritmo": "rapido", "gancho": "mano con anillo entrando a cuadro", "modelo_persona": "mano femenina",
    "motion_graphics": "Ninguno", "animacion": "Ninguna (live action)", "escenario_iluminacion": "fondo neutro, luz suave",
    "como_se_muestra_el_producto": "en la mano", "musica": "Sin musica", "voz_en_off": "No",
    "llamado_a_la_accion": "Implicito",
    "tomas": [
      {"n": 1, "tiempo": "0.00–1.60 s", "plano_y_camara": "primer plano · camara fija", "descripcion": "mano con el anillo"},
      {"n": 2, "tiempo": "1.60–3.00 s", "plano_y_camara": "plano detalle · zoom in", "descripcion": "el anillo de cerca"}
    ],
    "texto_en_pantalla": [], "dificil_de_replicar": ["texto legible sobre el producto"]
  },
  "visual_style": {"medium": "live_action", "format": "ugc", "aesthetics": ["clean"], "production_tier": "pro", "capture_device_guess": "smartphone"},
  "camera_summary": {"dominant_movement": "static", "static_share_pct": 80, "stabilization_overall": "tripod", "camera_personality": "estable"},
  "technical_signature": {"lens_family": "standard", "dof_overall": "moderate", "bokeh_overall": "suave", "key_overall": "high_key", "exposure_overall": "correct", "lighting_overall": {"setup": ["ring_light"], "key_direction": "front", "quality": "soft"}, "color_grade_overall": {"palette_hex": ["#112233"], "temperature_bias": "neutral", "saturation": "natural", "look": ["clean"]}},
  "effects_summary": [{"name": "flash", "count": 1, "time_ranges_s": ["1-2"], "purpose": "transicion", "made_with": "editor_preset"}],
  "transitions_summary": {"distribution": [{"type": "hard_cut", "count": 1}], "signature_transition": "hard_cut", "notes": "cortes secos"},
  "hook_and_structure": {"hook": {"type": "product_reveal", "time_range_s": "0.00-1.60", "description": "mano con anillo"}, "first_frame_description": "mano", "last_frame_description": "detalle", "retention_devices": ["texto"]},
  "graphics_summary": {"has_on_screen_text": false, "has_logo": false, "items": []},
  "talent": {"present": true, "people": [{"role": "hand_model", "appearance": "unas naturales"}]},
  "audio_observations": {
    "music": {"present": false, "genre": "", "tempo_bpm_estimate": null, "mood": "", "instrumentation": "", "energy_curve": "", "drops_and_breaks_s": []},
    "speech": {"present": false, "language": "", "type": "none", "summary": ""},
    "sfx": []
  },
  "editing_style": {"primary": "fast_cut", "secondary": [], "justification": "", "metrics": {"shot_count": 2, "cut_count": 1, "average_shot_length_s": 1.5, "median_shot_length_s": 1.5, "shortest_shot_s": 1.4, "longest_shot_s": 1.6, "cuts_per_second": 0.33, "micro_shot_count": 0, "pace_class": "fast", "pace_curve": {"quarters_asl_s": [1.5, 1.5, 1.5, 1.5], "trend": "steady"}, "beat_sync_ratio": null, "sync_elements": []}},
  "shots": [
    {"index": 1, "start_s": 0, "end_s": 1.6, "duration_s": 1.6, "micro_shot": false, "timing_precision": "approx", "shot_role": "hook", "shot_size": "close_up", "angle_vertical": "eye_level", "camera_movement": {"type": "static"}, "subject_action": "la mano gira el anillo", "product_visible": {"visible": true, "piece": "anillo", "how_shown": "hand_held"}, "transition_out": {"type": "hard_cut"}},
    {"index": 2, "start_s": 1.6, "end_s": 3.0, "duration_s": 1.4, "micro_shot": false, "timing_precision": "exact", "shot_role": "product", "shot_size": "detail", "angle_vertical": "high", "camera_movement": {"type": "zoom_in"}, "subject_action": "el anillo llena el cuadro", "product_visible": {"visible": true, "piece": "anillo", "how_shown": "in_use"}, "transition_out": {"type": "none_end"}}
  ],
  "quality": {"uncertainties": [], "assumptions": [], "checks_passed": true}
}`

func TestLoadVideoAnalyzerSystemPrompt(t *testing.T) {
	body, err := LoadVideoAnalyzerSystemPrompt()
	if err != nil {
		t.Fatalf("LoadVideoAnalyzerSystemPrompt() error = %v", err)
	}
	if strings.HasPrefix(body, "---") {
		t.Error("el system prompt todavia incluye el frontmatter")
	}
	if !strings.Contains(body, "DCS-ANALYZE") {
		t.Error("el system prompt no menciona DCS-ANALYZE")
	}
	if strings.Contains(body, "DCS-RECREATE") {
		t.Error("el system prompt no debe contener el skill de recreacion")
	}
	if !strings.Contains(body, "legacy_ui") {
		t.Error("el system prompt debe declarar el bloque legacy_ui")
	}
}

func TestParseSkillDocumentNormalizesCRLFAndBOM(t *testing.T) {
	raw := "\ufeff---\r\nname: DCS-ANALYZE\r\ndescription: test\r\n---\r\n\r\ncuerpo\r\n"
	name, body, err := parseSkillDocument(raw)
	if err != nil {
		t.Fatalf("parseSkillDocument() error = %v", err)
	}
	if name != analyzeSkillName {
		t.Errorf("name = %q, se esperaba %q", name, analyzeSkillName)
	}
	if body != "cuerpo" {
		t.Errorf("body = %q, se esperaba %q", body, "cuerpo")
	}
}

func TestLoadAnalyzeSkillRejectsWrongName(t *testing.T) {
	if _, err := loadAnalyzeSkill("---\nname: DCS-RECREATE\n---\ncuerpo\n"); err == nil {
		t.Fatal("se esperaba error con un name distinto de DCS-ANALYZE")
	}
}

func TestValidateAnalyzeDocumentAcceptsConsistentTimeline(t *testing.T) {
	doc := decodeAnalyzeDoc(t, sampleAnalyzeJSON)
	errs, warns := validateAnalyzeDocument(doc, nil)
	if len(errs) != 0 {
		t.Fatalf("errores inesperados: %v", errs)
	}
	if len(warns) != 0 {
		t.Fatalf("avisos inesperados: %v", warns)
	}
}

func TestValidateAnalyzeDocumentDetectsTimelineGap(t *testing.T) {
	broken := strings.Replace(sampleAnalyzeJSON, `"index": 2, "start_s": 1.6`, `"index": 2, "start_s": 1.2`, 1)
	doc := decodeAnalyzeDoc(t, broken)
	errs, _ := validateAnalyzeDocument(doc, nil)
	if !containsSubstring(errs, "shots[1].start_s") {
		t.Fatalf("se esperaba un error de hueco temporal, se obtuvo %v", errs)
	}
}

func TestValidateAnalyzeDocumentDetectsCountMismatch(t *testing.T) {
	broken := strings.Replace(sampleAnalyzeJSON, `"numero_de_tomas": 2`, `"numero_de_tomas": 3`, 1)
	doc := decodeAnalyzeDoc(t, broken)
	errs, _ := validateAnalyzeDocument(doc, nil)
	if !containsSubstring(errs, "legacy_ui.numero_de_tomas") {
		t.Fatalf("se esperaba un error de conteo, se obtuvo %v", errs)
	}
}

func TestValidateAnalyzeDocumentDetectsShortLastShot(t *testing.T) {
	broken := strings.Replace(sampleAnalyzeJSON, `"index": 2, "start_s": 1.6, "end_s": 3.0, "duration_s": 1.4`, `"index": 2, "start_s": 1.6, "end_s": 2.5, "duration_s": 0.9`, 1)
	doc := decodeAnalyzeDoc(t, broken)
	errs, _ := validateAnalyzeDocument(doc, nil)
	if !containsSubstring(errs, "video.duration_s") {
		t.Fatalf("se esperaba un error de duracion final, se obtuvo %v", errs)
	}
}

func TestValidateAnalyzeDocumentUsesMeasuredCuts(t *testing.T) {
	doc := decodeAnalyzeDoc(t, sampleAnalyzeJSON)
	// The only boundary the sample has is at 1.6 s, which ffmpeg also measured.
	if errs, _ := validateAnalyzeDocument(doc, []float64{1.6}); len(errs) != 0 {
		t.Fatalf("un corte medido que coincide no debe dar error: %v", errs)
	}
	// Two of three measured cuts are missing (1 fps under-segmentation).
	doc = decodeAnalyzeDoc(t, sampleAnalyzeJSON)
	errs, _ := validateAnalyzeDocument(doc, []float64{1.04, 1.6, 2.48})
	if !containsSubstring(errs, "cortes medidos") {
		t.Fatalf("se esperaba un error por cortes medidos sin frontera, se obtuvo %v", errs)
	}
}

func TestAnalyzePayloadSendsMeasuredCuts(t *testing.T) {
	meta := MediaMeta{DurationSec: 15.04, Width: 720, Height: 1280, FPS: 25, AspectRatio: "9:16", Cuts: []float64{1.08, 2.48}, CutsCount: 3}
	parts := analyzePayload(meta, []byte("v"))
	if parts[1]["type"] != "video_url" {
		t.Fatalf("el video debe ir como ultima parte (OpenRouter lo ignora si va primero): %v", parts[1])
	}
	text := parts[0]["text"].(string)
	if !strings.Contains(text, `"measured_cuts_s":[1.08,2.48]`) {
		t.Errorf("faltan los cortes medidos: %s", text)
	}
	if !strings.Contains(text, `"measured_shot_count":3`) {
		t.Errorf("falta measured_shot_count: %s", text)
	}
}

func TestReconcileTimelineSplitsAtMeasuredCuts(t *testing.T) {
	raw, changed := reconcileTimeline(json.RawMessage(sampleAnalyzeJSON), []float64{1.0, 2.0})
	if !changed {
		t.Fatal("se esperaba reconciliacion: hay cortes medidos dentro de las tomas del modelo")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("JSON reconciliado invalido: %v", err)
	}
	shots := out["shots"].([]any)
	if len(shots) != 4 {
		t.Fatalf("shots = %d, se esperaban 4 (0-1, 1-1.6, 1.6-2, 2-3)", len(shots))
	}
	first := shots[0].(map[string]any)
	if first["start_s"] != 0.0 || first["end_s"] != 1.0 {
		t.Errorf("primera toma = %v-%v", first["start_s"], first["end_s"])
	}
	if first["shot_size"] != "close_up" || first["camera_movement"].(map[string]any)["type"] != "static" {
		t.Errorf("la sub-toma debe heredar la ficha del plano que la contiene: %v", first)
	}
	last := shots[3].(map[string]any)
	if last["transition_out"].(map[string]any)["type"] != "none_end" {
		t.Errorf("la ultima toma debe ser none_end: %v", last["transition_out"])
	}
	if shots[1].(map[string]any)["transition_out"].(map[string]any)["type"] != "hard_cut" {
		t.Errorf("un corte medido interno debe ser hard_cut: %v", shots[1].(map[string]any)["transition_out"])
	}
	video := out["video"].(map[string]any)
	if video["shot_count"] != 4.0 {
		t.Errorf("video.shot_count = %v", video["shot_count"])
	}
	legacy := out["legacy_ui"].(map[string]any)
	if legacy["numero_de_tomas"] != 4.0 || len(legacy["tomas"].([]any)) != 4 {
		t.Errorf("legacy_ui desalineado: %v / %v", legacy["numero_de_tomas"], len(legacy["tomas"].([]any)))
	}
	metrics := out["editing_style"].(map[string]any)["metrics"].(map[string]any)
	if metrics["shot_count"] != 4.0 || metrics["cut_count"] != 3.0 {
		t.Errorf("metrics = %v", metrics)
	}
	quality := out["quality"].(map[string]any)
	if quality["timeline_source"] != "ffmpeg_measured" || quality["model_shots"] != 2.0 {
		t.Errorf("quality = %v", quality)
	}
	if len(quality["model_shots_list"].([]any)) != 2 {
		t.Errorf("se debe conservar la lista original del modelo: %v", quality["model_shots_list"])
	}
}

func TestReconcileTimelineExtendsShortModelTimeline(t *testing.T) {
	broken := strings.Replace(sampleAnalyzeJSON, `"duration_s": 3.0, "aspect_ratio"`, `"duration_s": 15.04, "aspect_ratio"`, 1)
	broken = strings.Replace(broken, `"index": 2, "start_s": 1.6, "end_s": 3.0, "duration_s": 1.4`, `"index": 2, "start_s": 1.6, "end_s": 8.0, "duration_s": 6.4`, 1)
	raw, changed := reconcileTimeline(json.RawMessage(broken), []float64{1.08, 12.0})
	if !changed {
		t.Fatal("se esperaba reconciliacion: el modelo no cubre los 15,04 s")
	}
	doc := &analyzeDocument{}
	if err := json.Unmarshal(raw, doc); err != nil {
		t.Fatalf("JSON reconciliado invalido: %v", err)
	}
	if last := doc.Shots[len(doc.Shots)-1]; last.EndS != 15.04 {
		t.Errorf("la linea de tiempo debe llegar al final del video, termina en %v", last.EndS)
	}
	errs, _ := validateAnalyzeDocument(doc, []float64{1.08, 12.0})
	if len(errs) != 0 {
		t.Fatalf("el documento reconciliado debe validar: %v", errs)
	}
}

func TestReconcileTimelineNoopWhenAligned(t *testing.T) {
	if _, changed := reconcileTimeline(json.RawMessage(sampleAnalyzeJSON), []float64{1.6}); changed {
		t.Fatal("no debe reconciliar si las fronteras ya coinciden con los cortes medidos")
	}
}

func TestAnalyzeWithSkillReconcilesMeasuredCuts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompletion(w, sampleAnalyzeJSON)
	}))
	defer srv.Close()
	m := &Module{brain: &brain{http: srv.Client()}}
	meta := MediaMeta{DurationSec: 3, Width: 1080, Height: 1920, FPS: 30, AspectRatio: "9:16", Cuts: []float64{1.0, 2.0}, CutsCount: 3}
	merged, _, err := m.analyzeWithSkill(context.Background(), srv.URL, "k", "google/gemini-3.8-flash", meta, []byte("v"))
	if err != nil {
		t.Fatalf("analyzeWithSkill() error = %v", err)
	}
	var out struct {
		Shots []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"shots"`
		LegacyUI struct {
			NumeroDeTomas float64 `json:"numero_de_tomas"`
		} `json:"legacy_ui"`
	}
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("breakdown invalido: %v", err)
	}
	if len(out.Shots) != 4 || out.LegacyUI.NumeroDeTomas != 4 {
		t.Fatalf("se esperaban 4 tomas reconciliadas, hubo %d (legacy %v)", len(out.Shots), out.LegacyUI.NumeroDeTomas)
	}
}

func TestMergeAnalyzeDocumentKeepsFrontContract(t *testing.T) {	doc := decodeAnalyzeDoc(t, sampleAnalyzeJSON)
	merged, err := mergeAnalyzeDocument(doc, json.RawMessage(sampleAnalyzeJSON))
	if err != nil {
		t.Fatalf("mergeAnalyzeDocument() error = %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(merged, &out); err != nil {
		t.Fatalf("el breakdown fusionado no es JSON: %v", err)
	}
	for _, key := range []string{
		"summary", "video_type", "duration_sec", "aspect_ratio", "pacing", "hook", "cuts_count",
		"shots", "has_person", "person", "has_motion_graphics", "motion_graphics", "on_screen_text",
		"animation_style", "setting_and_lighting", "product_presentation", "music", "voiceover",
		"call_to_action", "replication_notes", "schema_version", "legacy_ui", "dcs_analyze",
	} {
		if _, ok := out[key]; !ok {
			t.Errorf("falta la clave %q que consume el front", key)
		}
	}
	if out["summary"] != "Dos cortes rapidos sobre un anillo." {
		t.Errorf("summary = %v", out["summary"])
	}
	if out["pacing"] != "rapido" {
		t.Errorf("pacing = %v", out["pacing"])
	}
	shots, ok := out["shots"].([]any)
	if !ok || len(shots) != 2 {
		t.Fatalf("shots = %v", out["shots"])
	}
	first, _ := shots[0].(map[string]any)
	if first["start"] != 0.0 || first["end"] != 1.6 {
		t.Errorf("tiempos legacy incorrectos: %v", first)
	}
	if first["shot_type"] != "close_up" || first["camera_movement"] != "static" || first["transition_out"] != "hard_cut" {
		t.Errorf("campos legacy de la toma 1 incorrectos: %v", first)
	}
	if first["description"] != "mano con el anillo" {
		t.Errorf("description = %v", first["description"])
	}
	person, _ := out["person"].(map[string]any)
	if person["description"] != "unas naturales" {
		t.Errorf("person.description = %v", person["description"])
	}
	if out["has_person"] != true || out["cuts_count"] != 1.0 {
		t.Errorf("has_person/cuts_count = %v / %v", out["has_person"], out["cuts_count"])
	}
	dcs, _ := out["dcs_analyze"].(map[string]any)
	if dcs["schema_version"] != "dcs-analyze/1.0" {
		t.Errorf("dcs_analyze.schema_version = %v", dcs["schema_version"])
	}
}

func TestMissingAnalyzeSectionsDetectsTrimmedAnswer(t *testing.T) {
	trimmed := `{"schema_version":"dcs-analyze/1.0","video":{},"legacy_ui":{},"shots":[{"index":1}],"technical_signature":{},"effects_summary":[]}`
	missing := missingAnalyzeSections(json.RawMessage(trimmed))
	for _, key := range []string{"camera_summary", "editing_style", "transitions_summary", "effects_summary", "technical_signature"} {
		if !containsSubstring(missing, key) {
			t.Errorf("se esperaba %q como faltante, se obtuvo %v", key, missing)
		}
	}
	if got := missingAnalyzeSections(json.RawMessage(sampleAnalyzeJSON)); len(got) != 0 {
		t.Errorf("el JSON de prueba no debe tener secciones faltantes: %v", got)
	}
	attempt := parseAnalyzeAnswer(trimmed, nil)
	if !containsSubstring(attempt.errors, "faltan secciones del contrato") {
		t.Errorf("el parseo debe marcar el contrato incompleto: %v", attempt.errors)
	}
}

func TestAnalyzeModelDefaults(t *testing.T) {
	t.Setenv(analyzeModelEnv, "")
	if got := analyzeModel(""); got != analyzeDefaultModel {
		t.Errorf("sin modelo del front debe usar el default (%s), obtuvo %q", analyzeDefaultModel, got)
	}
	if got := analyzeModel("openrouter/google/gemini-4-argon"); got != "openrouter/google/gemini-4-argon" {
		t.Errorf("debe respetar el modelo del front, obtuvo %q", got)
	}
	t.Setenv(analyzeModelEnv, "google/gemini-9-test")
	if got := analyzeModel("openrouter/google/gemini-4-argon"); got != "google/gemini-9-test" {
		t.Errorf("la env debe ganar, obtuvo %q", got)
	}
}

func TestAnalyzeSkillFlagAndModelOverride(t *testing.T) {
	t.Setenv(analyzeSkillEnv, "legacy")
	if analyzeSkillEnabled() {
		t.Error("VIDEO_ANALYZER_SKILL=legacy debe desactivar el skill")
	}
	t.Setenv(analyzeSkillEnv, "dcs-analyze")
	if !analyzeSkillEnabled() {
		t.Error("el skill debe estar activo por defecto")
	}
	t.Setenv(analyzeModelEnv, "google/gemini-3.8-flash")
	if got := analyzeModel("openrouter/google/gemini-2.5-pro"); got != "google/gemini-3.8-flash" {
		t.Errorf("analyzeModel() = %q", got)
	}
	t.Setenv(analyzeModelEnv, "")
	if got := analyzeModel("openrouter/google/gemini-2.5-pro"); got != "openrouter/google/gemini-2.5-pro" {
		t.Errorf("analyzeModel() = %q", got)
	}
}

func TestAnalyzeWithSkillRetriesOnceAndMerges(t *testing.T) {
	broken := strings.Replace(sampleAnalyzeJSON, `"index": 2, "start_s": 1.6`, `"index": 2, "start_s": 1.2`, 1)
	var calls int
	var firstBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if calls == 1 {
			_ = json.Unmarshal(body, &firstBody)
			writeCompletion(w, broken)
			return
		}
		writeCompletion(w, sampleAnalyzeJSON)
	}))
	defer srv.Close()

	m := &Module{brain: &brain{http: srv.Client()}}
	meta := MediaMeta{DurationSec: 3, Width: 1080, Height: 1920, FPS: 30, AspectRatio: "9:16"}
	merged, u, err := m.analyzeWithSkill(context.Background(), srv.URL, "test-key", "openrouter/google/gemini-3.8-flash", meta, []byte("video"))
	if err != nil {
		t.Fatalf("analyzeWithSkill() error = %v", err)
	}
	if calls != 2 {
		t.Errorf("se esperaban 2 llamadas (original + reintento), hubo %d", calls)
	}
	if u.CompletionTokens != 20 {
		t.Errorf("usage acumulado inesperado: %+v", u)
	}
	if !strings.Contains(string(merged), "Dos cortes rapidos") {
		t.Errorf("el breakdown fusionado no trae el resumen: %s", merged)
	}

	// First call: video first, then the measured params; JSON mode requested.
	msgs, _ := firstBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("se esperaban 2 mensajes, hubo %d", len(msgs))
	}
	system, _ := msgs[0].(map[string]any)
	if !strings.Contains(system["content"].(string), "DCS-ANALYZE") {
		t.Error("el system prompt no es el skill")
	}
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("se esperaban 2 partes del usuario, hubo %d", len(parts))
	}
	if parts[1].(map[string]any)["type"] != "video_url" {
		t.Errorf("el video debe ir como ultima parte, se obtuvo %v", parts[1].(map[string]any)["type"])
	}
	paramsPart := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(paramsPart, `"sampling_fps":1`) || !strings.Contains(paramsPart, `"max_shots":120`) {
		t.Errorf("faltan parametros medidos: %s", paramsPart)
	}
	format, _ := firstBody["response_format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Errorf("response_format = %v", firstBody["response_format"])
	}
}

func TestChatWithOptionsRejectsTruncatedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "The chain from @Image1 rests"},
				"finish_reason": "length",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 9000},
		})
	}))
	defer srv.Close()
	b := &brain{http: srv.Client()}
	if _, _, err := b.chatWithOptions(context.Background(), srv.URL, "k", "m", "sys", []map[string]any{textPart("hola")}, 9000, chatOptions{}); err == nil {
		t.Fatal("una respuesta truncada debe devolver error")
	}
}

func TestAnalyzeWithSkillFailsWithoutJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCompletion(w, "no puedo analizar este video")
	}))
	defer srv.Close()
	m := &Module{brain: &brain{http: srv.Client()}}
	if _, _, err := m.analyzeWithSkill(context.Background(), srv.URL, "test-key", "google/gemini-3.8-flash", MediaMeta{DurationSec: 1}, []byte("v")); err == nil {
		t.Fatal("se esperaba error para que el llamador caiga al analizador anterior")
	}
}

func writeCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "cost": 0.0},
	})
}

func decodeAnalyzeDoc(t *testing.T, raw string) *analyzeDocument {
	t.Helper()
	doc := &analyzeDocument{}
	if err := json.Unmarshal([]byte(raw), doc); err != nil {
		t.Fatalf("JSON de prueba invalido: %v", err)
	}
	return doc
}

func containsSubstring(values []string, needle string) bool {
	for _, v := range values {
		if strings.Contains(v, needle) {
			return true
		}
	}
	return false
}
