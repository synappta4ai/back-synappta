package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseArtifactKind(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind     string
		wantStep int
		wantOK   bool
	}{
		{artifactSalesAngles, 1, true},
		{artifactScenePrompts, 1, true},
		{artifactSceneAnalysis, 1, true},
		{artifactStoryboard, 2, true},
		{artifactClipPrompt, 3, true},
		{"video_final", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		spec, ok := parseArtifactKind(tc.kind)
		if ok != tc.wantOK {
			t.Errorf("%q: ok=%v, want %v", tc.kind, ok, tc.wantOK)
		}
		if ok && spec.Step != tc.wantStep {
			t.Errorf("%q: step=%d, want %d", tc.kind, spec.Step, tc.wantStep)
		}
	}
}

func TestStepArtifact(t *testing.T) {
	t.Parallel()

	cases := []struct {
		workflow string
		step     int
		wantKind string
	}{
		{workflowCommercial, 1, artifactScenePrompts},
		{workflowRealEstate, 1, artifactSalesAngles},
		{workflowCinema, 1, artifactSceneAnalysis},
		{workflowCommercial, 2, artifactStoryboard},
		{workflowCinema, 3, artifactClipPrompt},
	}
	for _, tc := range cases {
		if got := stepArtifact(tc.workflow, tc.step); got.Kind != tc.wantKind {
			t.Errorf("%s paso %d = %q, want %q", tc.workflow, tc.step, got.Kind, tc.wantKind)
		}
	}
	if got := stepArtifact(workflowCinema, 9); got.Kind != "" {
		t.Errorf("paso 9 = %q, quiero vacío", got.Kind)
	}
}

func TestExtractArtifactJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{"json puro", `{"paso":1,"estado":"ok"}`, true},
		{"fence markdown", "```json\n{\"paso\":1}\n```", true},
		{"prosa alrededor", "Acá va:\n{\"paso\":1}\nListo.", true},
		{"sin json", "No puedo ayudarte.", false},
		{"json vacío", `{}`, false},
		{"json inválido", `{paso:1}`, false},
		{"solo corchetes", `[1,2]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, ok := extractArtifactJSON(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v", ok, tc.wantOK)
			}
			if ok && data == nil {
				t.Fatal("data nil con ok=true")
			}
		})
	}
}

func TestDecodePayload(t *testing.T) {
	t.Parallel()

	if got := decodePayload(nil); len(got) != 0 {
		t.Errorf("payload vacío = %v", got)
	}
	if got := decodePayload(json.RawMessage(`{"cantidad_angulos":2}`)); got["cantidad_angulos"] != float64(2) {
		t.Errorf("cantidad_angulos = %v", got["cantidad_angulos"])
	}
	if got := decodePayload(json.RawMessage(`no es json`)); len(got) != 0 {
		t.Errorf("payload inválido = %v", got)
	}
}

func TestValidateSalesAngles(t *testing.T) {
	t.Parallel()

	okData := map[string]any{
		"estado": "ok",
		"angulos": []any{
			map[string]any{"id": "A1", "titulo": "Cerca de todo", "promesa": "Llegas al metro en 7 minutos."},
			map[string]any{"id": "A2", "titulo": "Jardines", "promesa": "Volvés a un conjunto con jardines."},
		},
	}
	payload := map[string]any{"cantidad_angulos": float64(2)}

	if warns := validateArtifact(artifactSpec{Kind: artifactSalesAngles, Step: 1}, okData, payload); len(warns) != 0 {
		t.Errorf("caso ok con advertencias: %v", warns)
	}

	cases := []struct {
		name      string
		data      map[string]any
		payload   map[string]any
		wantInAny string
	}{
		{
			name:      "cantidad incorrecta",
			data:      okData,
			payload:   map[string]any{"cantidad_angulos": float64(3)},
			wantInAny: "pediste 3",
		},
		{
			name: "sin titulo",
			data: map[string]any{"estado": "ok", "angulos": []any{
				map[string]any{"promesa": "algo"},
			}},
			payload:   map[string]any{},
			wantInAny: "titulo",
		},
		{
			name: "promesa financiera prohibida",
			data: map[string]any{"estado": "ok", "angulos": []any{
				map[string]any{"titulo": "Inversión", "promesa": "Garantizado: plusvalía del 10% anual."},
			}},
			payload:   map[string]any{},
			wantInAny: "financiera",
		},
		{
			name:      "sin angulos",
			data:      map[string]any{"estado": "ok"},
			payload:   map[string]any{},
			wantInAny: "angulos",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			warns := validateArtifact(artifactSpec{Kind: artifactSalesAngles, Step: 1}, tc.data, tc.payload)
			if !containsSubstr(warns, tc.wantInAny) {
				t.Fatalf("advertencias %v sin %q", warns, tc.wantInAny)
			}
		})
	}

	// Estado necesita_informacion no se valida: el turno va por questions.
	if warns := validateArtifact(
		artifactSpec{Kind: artifactSalesAngles, Step: 1},
		map[string]any{"estado": "necesita_informacion"},
		payload,
	); len(warns) != 0 {
		t.Errorf("necesita_informacion con advertencias: %v", warns)
	}
}

func TestValidateScenePrompts(t *testing.T) {
	t.Parallel()

	okData := map[string]any{
		"estado": "ok",
		"prompts": []any{
			map[string]any{"id": "P1", "beats": []any{map[string]any{"momento": float64(1)}}},
			map[string]any{"id": "P2", "beats": []any{map[string]any{"momento": float64(1)}}},
		},
	}
	payload := map[string]any{"cantidad_prompts": float64(2)}

	if warns := validateArtifact(artifactSpec{Kind: artifactScenePrompts, Step: 1}, okData, payload); len(warns) != 0 {
		t.Errorf("caso ok con advertencias: %v", warns)
	}

	cases := []struct {
		name      string
		data      map[string]any
		payload   map[string]any
		cinema    bool
		wantInAny string
	}{
		{
			name:      "cantidad incorrecta",
			data:      okData,
			payload:   map[string]any{"cantidad_prompts": float64(3)},
			wantInAny: "pediste 3",
		},
		{
			name: "beats vacío",
			data: map[string]any{"estado": "ok", "prompts": []any{
				map[string]any{"id": "P1", "beats": []any{}},
			}},
			payload:   map[string]any{},
			wantInAny: "beats",
		},
		{
			name: "cine sin biblia",
			data: map[string]any{"estado": "ok", "prompts": []any{
				map[string]any{"id": "P1", "beats": []any{map[string]any{}}},
			}},
			payload:   map[string]any{},
			cinema:    true,
			wantInAny: "biblia_continuidad",
		},
		{
			name:      "sin prompts",
			data:      map[string]any{"estado": "ok"},
			payload:   map[string]any{},
			wantInAny: "prompts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind := artifactScenePrompts
			if tc.cinema {
				kind = artifactSceneAnalysis
			}
			warns := validateArtifact(artifactSpec{Kind: kind, Step: 1}, tc.data, tc.payload)
			if !containsSubstr(warns, tc.wantInAny) {
				t.Fatalf("advertencias %v sin %q", warns, tc.wantInAny)
			}
		})
	}

	// Cine con biblia completa valida OK.
	cinemaOK := map[string]any{
		"estado": "ok",
		"biblia_continuidad": map[string]any{
			"personajes": []any{map[string]any{"descripcion": "mujer, 40", "referencia": "mujer.jpg"}},
		},
		"prompts": okData["prompts"],
	}
	if warns := validateArtifact(artifactSpec{Kind: artifactSceneAnalysis, Step: 1}, cinemaOK, payload); len(warns) != 0 {
		t.Errorf("cine ok con advertencias: %v", warns)
	}
}

func TestValidateStoryboard(t *testing.T) {
	t.Parallel()

	okData := map[string]any{
		"estado":                 "ok",
		"duracion_total_s":       float64(9),
		"prompt_hoja_storyboard": "Hoja de storyboard a lápiz…",
		"vinetas": []any{
			map[string]any{"n": float64(1), "duracion_s": float64(3)},
			map[string]any{"n": float64(2), "duracion_s": float64(3)},
			map[string]any{"n": float64(3), "duracion_s": float64(3)},
		},
	}
	payload := map[string]any{
		"cantidad_vinetas":         float64(3),
		"duracion_total_s":         float64(9),
		"duracion_minima_vineta_s": float64(1.5),
	}

	if warns := validateArtifact(artifactSpec{Kind: artifactStoryboard, Step: 2}, okData, payload); len(warns) != 0 {
		t.Errorf("caso ok con advertencias: %v", warns)
	}

	cases := []struct {
		name      string
		data      map[string]any
		wantInAny string
	}{
		{
			name: "cantidad incorrecta",
			data: withMap(okData, "vinetas", append(okData["vinetas"].([]any),
				map[string]any{"n": float64(4), "duracion_s": float64(1)})),
			wantInAny: "pediste 3",
		},
		{
			name: "suma incorrecta",
			data: withMap(okData, "vinetas", []any{
				map[string]any{"n": float64(1), "duracion_s": float64(4)},
				map[string]any{"n": float64(2), "duracion_s": float64(2)},
				map[string]any{"n": float64(3), "duracion_s": float64(2.5)},
			}),
			wantInAny: "suman",
		},
		{
			name: "bajo el mínimo",
			data: withMap(okData, "vinetas", []any{
				map[string]any{"n": float64(1), "duracion_s": float64(1)},
				map[string]any{"n": float64(2), "duracion_s": float64(3)},
				map[string]any{"n": float64(3), "duracion_s": float64(4)},
			}),
			wantInAny: "mínimo",
		},
		{
			name: "duraciones desiguales",
			data: withMap(okData, "vinetas", []any{
				map[string]any{"n": float64(1), "duracion_s": float64(4)},
				map[string]any{"n": float64(2), "duracion_s": float64(3)},
				map[string]any{"n": float64(3), "duracion_s": float64(2)},
			}),
			wantInAny: "lo mismo",
		},
		{
			name:      "sin hoja",
			data:      withMap(okData, "prompt_hoja_storyboard", ""),
			wantInAny: "prompt_hoja_storyboard",
		},
		{
			name: "viñeta sin duración",
			data: withMap(okData, "vinetas", []any{
				map[string]any{"n": float64(1), "duracion_s": float64(3)},
				map[string]any{"n": float64(2), "duracion_s": float64(2)},
				map[string]any{"n": float64(3)},
			}),
			wantInAny: "duracion_s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			warns := validateArtifact(artifactSpec{Kind: artifactStoryboard, Step: 2}, tc.data, payload)
			if !containsSubstr(warns, tc.wantInAny) {
				t.Fatalf("advertencias %v sin %q", warns, tc.wantInAny)
			}
		})
	}

	// Sin vinetas el contrato falla siempre.
	if warns := validateArtifact(
		artifactSpec{Kind: artifactStoryboard, Step: 2},
		map[string]any{"estado": "ok"},
		payload,
	); !containsSubstr(warns, "vinetas") {
		t.Errorf("advertencias %v sin \"vinetas\"", warns)
	}
}

func TestValidateScenePromptTags(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"cantidad_prompts": float64(1),
		"referencias_subidas": []any{
			map[string]any{"archivo": "Tienda.jpg", "tag": "@tienda", "funcion": "locacion"},
		},
	}
	okData := map[string]any{
		"estado": "ok",
		"prompts": []any{
			map[string]any{
				"titulo":   "La calle a las 7",
				"locacion": "@tienda",
				"beats":    []any{map[string]any{"accion_fisica": "abre la puerta"}},
			},
		},
	}
	spec := artifactSpec{Kind: artifactScenePrompts, Step: 1}
	if warns := validateArtifact(spec, okData, payload); len(warns) != 0 {
		t.Errorf("caso ok con advertencias: %v", warns)
	}

	// Un tag inventado no adjunta ninguna imagen al video.
	invented := withMap(okData, "prompts", []any{
		map[string]any{
			"locacion": "@abuelo",
			"beats":    []any{map[string]any{"accion_fisica": "saluda"}},
		},
	})
	if warns := validateArtifact(spec, invented, payload); !containsSubstr(warns, "@abuelo") {
		t.Errorf("advertencias %v sin @abuelo", warns)
	}

	// Sin referencias subidas no opina sobre los tags.
	noRefs := validateArtifact(spec, invented, map[string]any{"cantidad_prompts": float64(1)})
	if len(noRefs) != 0 {
		t.Errorf("sin referencias no debería advertir: %v", noRefs)
	}

	if warns := validateArtifact(spec, map[string]any{"estado": "ok"}, payload); !containsSubstr(warns, "prompts") {
		t.Errorf("advertencias %v sin \"prompts\"", warns)
	}
	wrongCount := withMap(okData, "prompts", []any{
		map[string]any{"beats": []any{map[string]any{"accion_fisica": "x"}}},
		map[string]any{"beats": []any{map[string]any{"accion_fisica": "y"}}},
	})
	if warns := validateArtifact(spec, wrongCount, payload); !containsSubstr(warns, "pediste 1") {
		t.Errorf("advertencias %v sin conteo", warns)
	}
	noBeats := withMap(okData, "prompts", []any{map[string]any{"locacion": "@tienda"}})
	if warns := validateArtifact(spec, noBeats, payload); !containsSubstr(warns, "beats") {
		t.Errorf("advertencias %v sin beats", warns)
	}

	// Cine exige además la biblia de continuidad.
	cinema := artifactSpec{Kind: artifactSceneAnalysis, Step: 1}
	if warns := validateArtifact(cinema, okData, payload); !containsSubstr(warns, "biblia_continuidad") {
		t.Errorf("advertencias %v sin biblia", warns)
	}
}

func TestValidateClipPrompt(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"duracion_total_s": float64(8),
		"referencias": []any{
			map[string]any{"tag": "@image1"},
			map[string]any{"tag": "@image2"},
		},
	}
	okData := map[string]any{
		"estado":           "ok",
		"duracion_total_s": float64(8),
		"prompt_video":     "0-3 s: plano general. Usa @image1 como guía. 3-8 s: primer plano, @image2 define la fachada.",
	}
	if warns := validateArtifact(artifactSpec{Kind: artifactClipPrompt, Step: 3}, okData, payload); len(warns) != 0 {
		t.Errorf("caso ok con advertencias: %v", warns)
	}

	cases := []struct {
		name      string
		data      map[string]any
		wantInAny string
	}{
		{
			name:      "sin prompt_video",
			data:      map[string]any{"estado": "ok"},
			wantInAny: "prompt_video",
		},
		{
			name: "referencia sin declarar",
			data: map[string]any{
				"estado":           "ok",
				"duracion_total_s": float64(8),
				"prompt_video":     "0-8 s: toma única con @image1.",
			},
			wantInAny: "@image2",
		},
		{
			name: "tiempos no cubren el total",
			data: map[string]any{
				"estado":           "ok",
				"duracion_total_s": float64(8),
				"prompt_video":     "0-3 s: @image1 y @image2.",
			},
			wantInAny: "llegan a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			warns := validateArtifact(artifactSpec{Kind: artifactClipPrompt, Step: 3}, tc.data, payload)
			if !containsSubstr(warns, tc.wantInAny) {
				t.Fatalf("advertencias %v sin %q", warns, tc.wantInAny)
			}
		})
	}

	// Sin rangos de tiempo reconocibles no se queja de tiempos.
	noTimes := map[string]any{
		"estado":           "ok",
		"duracion_total_s": float64(8),
		"prompt_video":     "Usa @image1 y @image2 como referencias.",
	}
	if warns := validateArtifact(artifactSpec{Kind: artifactClipPrompt, Step: 3}, noTimes, payload); len(warns) != 0 {
		t.Errorf("sin rangos no debería advertir: %v", warns)
	}
}

func TestArtifactQuestions(t *testing.T) {
	t.Parallel()

	if qs := artifactQuestions(map[string]any{"estado": "ok"}); qs != nil {
		t.Errorf("estado ok devolvió preguntas: %v", qs)
	}

	data := map[string]any{
		"estado":               "necesita_informacion",
		"preguntas_pendientes": []any{"¿Cuál es el precio? (sugerí un rango)", "  ", 42},
	}
	qs := artifactQuestions(data)
	if len(qs) != 1 || !strings.Contains(qs[0], "precio") {
		t.Errorf("preguntas = %v", qs)
	}
}

func TestNormalizeSalesAngles(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"angulos": []any{
			map[string]any{
				"titulo":          "Cerca de todo",
				"gancho_cierre":   "Ven a visitarlo",
				"promesa":         "Metro a 7 minutos.",
				"comprador_ideal": "Jóvenes de 30",
			},
			map[string]any{"promesa": "Sin título"},
		},
	}
	normalizeSalesAngles(data)

	legacy, ok := data["angles"].([]map[string]any)
	if !ok || len(legacy) != 2 {
		t.Fatalf("angles = %v", data["angles"])
	}
	first := legacy[0]
	if first["title"] != "Cerca de todo" || first["hook"] != "Ven a visitarlo" ||
		first["narrative_pitch"] != "Metro a 7 minutos." || first["target_audience"] != "Jóvenes de 30" {
		t.Errorf("legacy = %v", first)
	}
	if first["selected"] != false {
		t.Errorf("selected = %v", first["selected"])
	}
	// Sin título usa el default numerado.
	if legacy[1]["title"] != "Ángulo 2" {
		t.Errorf("title default = %v", legacy[1]["title"])
	}
}

func TestAppendWarnings(t *testing.T) {
	t.Parallel()

	data := map[string]any{"advertencias": []any{"ya existía"}}
	appendWarnings(data, []string{"nueva 1", "nueva 2"})
	warns, _ := data["advertencias"].([]string)
	if len(warns) != 3 {
		t.Fatalf("advertencias = %v", data["advertencias"])
	}
}

func TestCorrectionMessage(t *testing.T) {
	t.Parallel()

	msg := correctionMessage([]string{"falta titulo", "beats vacío"})
	if !strings.Contains(msg, "falta titulo") || !strings.Contains(msg, "beats vacío") {
		t.Errorf("mensaje = %q", msg)
	}
}

func TestArtifactUserMessage(t *testing.T) {
	t.Parallel()

	req := chatRequest{
		Message: "Mirá mi brief",
		Payload: json.RawMessage(`{"cantidad_angulos":2}`),
	}
	msg := artifactUserMessage(req, artifactSpec{Kind: artifactSalesAngles, Step: 1})
	for _, want := range []string{"Mirá mi brief", "cantidad_angulos", "SALIDA", "sales_angles"} {
		if !strings.Contains(msg, want) {
			t.Errorf("mensaje sin %q: %q", want, msg)
		}
	}

	// Sin payload, solo el mensaje del usuario (ruta legacy).
	legacy := artifactUserMessage(chatRequest{Message: "creeme 3 ángulos"}, artifactSpec{Kind: artifactSalesAngles, Step: 1})
	if !strings.Contains(legacy, "creeme 3 ángulos") || strings.Contains(legacy, "Entrada del paso") {
		t.Errorf("mensaje legacy = %q", legacy)
	}
}

// containsSubstr busca un subtexto en cualquier advertencia.
func containsSubstr(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// withMap copia un map de artefacto reemplazando un campo (sin mutar el
// original que comparten los casos).
func withMap(m map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}
