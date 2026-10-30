package infoproduct

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadRecreateSystemPrompt(t *testing.T) {
	prompt, err := LoadRecreateSystemPrompt()
	if err != nil {
		t.Fatalf("LoadRecreateSystemPrompt: %v", err)
	}
	if strings.TrimSpace(prompt) == "" {
		t.Fatal("el skill cargado esta vacio")
	}
	if strings.HasPrefix(strings.TrimSpace(prompt), "---") {
		t.Fatal("el frontmatter no se elimino")
	}
	if !strings.Contains(prompt, "DCS-RECREATE") {
		t.Fatal("el skill no contiene DCS-RECREATE")
	}
	for _, want := range []string{"LEAD LINE", "REFERENCE ROLES", "SHOT LIST", "Frame:", "Action:", "Cut:", "LOOK"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("el skill no contiene la seccion %q", want)
		}
	}
	for _, forbidden := range []string{"AUTOMATED PIPELINE OVERRIDE", "Camera Capture:"} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("el skill conserva la version anterior (%q)", forbidden)
		}
	}
	meta := inspectRecreateSkill(recreateSkillRaw)
	if meta.Name != recreateSkillName {
		t.Fatalf("name embebido = %q, se esperaba %q", meta.Name, recreateSkillName)
	}
	if meta.Bytes == 0 || len(meta.SHA256) != 64 {
		t.Fatalf("metadatos invalidos: %+v", meta)
	}
}

func TestRecreateSkillValidation(t *testing.T) {
	good := "\ufeff---\r\nname: DCS-RECREATE\r\ndescription: \"x\"\r\n---\r\n\r\nBODY\r\n"
	body, err := loadRecreateSkill(good)
	if err != nil {
		t.Fatalf("BOM/CRLF debe aceptarse: %v", err)
	}
	if body != "BODY" {
		t.Fatalf("cuerpo inesperado: %q", body)
	}
	if _, err := loadRecreateSkill("---\nname: Synappta-DIRECTION description: \"old\"\n---\nbody"); err == nil {
		t.Fatal("un name distinto de DCS-RECREATE debe fallar")
	}
	if _, err := loadRecreateSkill("---\nname: DCS-RECREATE\n---\nCamera Capture: x"); err == nil {
		t.Fatal("Camera Capture: debe rechazarse")
	}
	if _, err := loadRecreateSkill("---\nname: DCS-RECREATE\nbody"); err == nil {
		t.Fatal("un frontmatter sin cierre debe fallar")
	}
}

func TestRecreateSkillEnabled(t *testing.T) {
	t.Setenv("VIDEO_RECREATE_SKILL", "")
	if !recreateSkillEnabled() {
		t.Fatal("sin variable el skill debe estar activo")
	}
	t.Setenv("VIDEO_RECREATE_SKILL", "legacy")
	if recreateSkillEnabled() {
		t.Fatal("legacy debe desactivar el skill")
	}
	t.Setenv("VIDEO_RECREATE_SKILL", "dcs-recreate")
	if !recreateSkillEnabled() {
		t.Fatal("dcs-recreate debe mantener el skill activo")
	}
}

func TestBuildRecreateUserMessage(t *testing.T) {
	breakdown := json.RawMessage(`{
		"summary": "Un video de producto",
		"pacing": "fast",
		"setting_and_lighting": "cocina luminosa",
		"shots": [{"index":1,"start":0,"end":2,"shot_type":"close-up","camera_movement":"pan","description":"botella","transition_out":"cut"}],
		"person": {"description": "mujer"},
		"music": {"present": true},
		"voiceover": {"present": true},
		"on_screen_text": [{"text": "Hola"}],
		"motion_graphics": "flechas",
		"replication_notes": ["dificil"]
	}`)
	in := recreateInput{
		Breakdown:     breakdown,
		Images:        []recreateImage{{FileID: "i1", Role: "product", Name: "botella.png"}, {FileID: "i2", Role: "person", Name: "modelo.png"}},
		Audios:        []recreateAudio{{FileID: "a1", Name: "beat.mp3"}},
		Notes:         "sin logo visible",
		AspectRatio:   "9:16",
		FPS:           30,
		TotalDuration: 12.5,
		MaxDuration:   15,
		TargetModel:   "seedance",
	}
	msg, err := buildRecreateUserMessage(in)
	if err != nil {
		t.Fatalf("buildRecreateUserMessage: %v", err)
	}

	order := []string{"INGREDIENTS:", "BREAKDOWN:", "USER_NOTES:", "PARAMS:"}
	last := -1
	for _, block := range order {
		idx := strings.Index(msg, block)
		if idx < 0 {
			t.Fatalf("falta el bloque %s", block)
		}
		if idx <= last {
			t.Fatalf("bloque %s fuera de orden", block)
		}
		last = idx
	}

	for _, want := range []string{
		"@Image1 = product: the user's product (botella.png)",
		"@Image2 = identity reference: the on-screen person (modelo.png)",
		"@Audio1 = soundtrack / rhythm reference (beat.mp3)",
		"max_duration_s: 15",
		"min_shot_s: 0.30",
		"max_words: 1600",
		"total_duration_s: 12.5",
		"target_model: seedance",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("falta %q en el mensaje", want)
		}
	}
	for _, forbidden := range []string{"TASK:", "STYLE_RULES:", "OUTPUT:"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("el mensaje conserva la instruccion anterior %q", forbidden)
		}
	}
	if !strings.HasPrefix(msg, "INGREDIENTS:") {
		t.Errorf("el mensaje debe empezar por INGREDIENTS: %q", msg[:40])
	}

	for _, forbidden := range []string{`"person":`, `"music":`, `"voiceover":`, `"on_screen_text":`, `"motion_graphics":`, `"replication_notes":`} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("el desglose conserva el campo prohibido %s", forbidden)
		}
	}
	for _, kept := range []string{`"summary":`, `"pacing":`, `"setting_and_lighting":`, `"shots":`, `"description":`} {
		if !strings.Contains(msg, kept) {
			t.Errorf("el desglose perdio el campo %s", kept)
		}
	}

	noNotes := in
	noNotes.Notes = ""
	msg2, err := buildRecreateUserMessage(noNotes)
	if err != nil {
		t.Fatalf("buildRecreateUserMessage sin notas: %v", err)
	}
	if strings.Contains(msg2, "USER_NOTES:") {
		t.Error("USER_NOTES debe omitirse cuando esta vacio")
	}
}

func TestSanitizeBreakdownIncludesTechnicalSheet(t *testing.T) {
	raw := json.RawMessage(`{
	  "summary": "resumen",
	  "pacing": "ritmo",
	  "setting_and_lighting": "estudio",
	  "person": {"description": "no debe pasar"},
	  "dcs_analyze": {
	    "schema_version": "dcs-analyze/1.0",
	    "talent": {"present": true, "people": [{"appearance": "no debe pasar"}]},
	    "brand_elements": [{"type": "logo"}],
	    "graphics_summary": {"items": []},
	    "audio_observations": {"music": {"present": true}},
	    "technical_signature": {"lens_family": "35mm prime", "dof_overall": "shallow"},
	    "camera_summary": {"dominant_movement": "static"},
	    "editing_style": {"primary": "fast_cut", "metrics": {"average_shot_length_s": 0.71}},
	    "transitions_summary": {"distribution": [{"type": "hard_cut", "count": 20}]},
	    "effects_summary": [{"name": "bloom"}],
	    "recreation_blueprint": {"timing_template_s": [0.83, 1.67]},
	    "visual_style": {"medium": "live_action"},
	    "shots": [
	      {"index": 1, "start_s": 0, "end_s": 0.83, "shot_size": "close_up",
	       "camera_movement": {"type": "handheld"}, "lens": {"class": "35mm"},
	       "lighting": {"setup": "ring_light"}, "subject_action": "gira el anillo",
	       "transition_out": {"type": "hard_cut"}, "appearance": "no debe pasar"}
	    ]
	  }
	}`)
	out := sanitizeBreakdown(raw)
	for _, want := range []string{"technical_signature", "camera_summary", "editing_style", "recreation_blueprint", "timing_template_s", "35mm prime", "subject_action", "gira el anillo"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en el breakdown tecnico", want)
		}
	}
	for _, banned := range []string{"talent", "brand_elements", "audio_observations", "no debe pasar", "graphics_summary"} {
		if strings.Contains(out, banned) {
			t.Errorf("no debe incluir %q", banned)
		}
	}
}

func TestSanitizeBreakdownFallsBackToLegacyShape(t *testing.T) {
	raw := json.RawMessage(`{"summary":"s","pacing":"p","setting_and_lighting":"l","shots":[{"index":1,"start":0,"end":1,"shot_type":"close-up"}]}`)
	out := sanitizeBreakdown(raw)
	for _, want := range []string{"\"summary\"", "\"pacing\"", "\"setting_and_lighting\"", "\"shots\"", "close-up"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en el breakdown legacy", want)
		}
	}
	if strings.Contains(out, "technical") {
		t.Error("sin dcs_analyze no debe inventar un bloque technical")
	}
}

func TestBuildRecreateUserMessageDefaults(t *testing.T) {
	in := recreateInput{
		Breakdown: json.RawMessage(`{"summary":"s"}`),
		Images:    []recreateImage{{FileID: "i1"}},
	}
	msg, err := buildRecreateUserMessage(in)
	if err != nil {
		t.Fatalf("buildRecreateUserMessage: %v", err)
	}
	for _, want := range []string{
		"@Image1 = product: the user's product",
		"aspect_ratio: 9:16",
		"fps: 24",
		"max_duration_s: 15",
		"min_shot_s: 0.30",
		"max_words: 1600",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("falta el default %q", want)
		}
	}
}

func TestBuildRecreateUserMessageMessikaOrder(t *testing.T) {
	in := recreateInput{
		Breakdown: json.RawMessage(`{"summary":"messika"}`),
		Images: []recreateImage{
			{FileID: "p", Role: "person", Name: "moreno.jpg"},
			{FileID: "c1", Role: "product", Name: "cadena-1.jpg"},
			{FileID: "c2", Role: "product", Name: "cadena-2.jpg"},
			{FileID: "c3", Role: "product", Name: "cadena-3.jpg"},
			{FileID: "c4", Role: "product", Name: "cadena-4.jpg"},
		},
		Audios: []recreateAudio{{FileID: "a", Name: "audio.mp3"}},
	}
	msg, err := buildRecreateUserMessage(in)
	if err != nil {
		t.Fatalf("buildRecreateUserMessage: %v", err)
	}
	for _, want := range []string{
		"@Image1 = identity reference: the on-screen person (moreno.jpg)",
		"@Image2 = product: the user's product (cadena-1.jpg)",
		"@Image3 = product: the user's product (cadena-2.jpg)",
		"@Image4 = product: the user's product (cadena-3.jpg)",
		"@Image5 = product: the user's product (cadena-4.jpg)",
		"@Audio1 = soundtrack / rhythm reference (audio.mp3)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("falta %q en el manifiesto", want)
		}
	}
}

func TestBuildRecreateUserMessageRequiresIngredients(t *testing.T) {
	_, err := buildRecreateUserMessage(recreateInput{Breakdown: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("sin ingredientes debe fallar")
	}
}

func TestCleanRecreatePrompt(t *testing.T) {
	got, err := cleanRecreatePrompt("```markdown\nHere is the prompt:\n\n0-2s: close-up of @Image1.\n2-4s: pan.\n```")
	if err != nil {
		t.Fatalf("cleanRecreatePrompt: %v", err)
	}
	if strings.Contains(got, "```") || strings.Contains(got, "Here is the prompt") {
		t.Fatalf("no se limpio el prompt: %q", got)
	}
	if !strings.HasPrefix(got, "0-2s:") {
		t.Fatalf("prompt inesperado: %q", got)
	}
	if _, err := cleanRecreatePrompt("```\n```"); err == nil {
		t.Fatal("un prompt vacio debe fallar")
	}
}

func TestLooksLikeQuestion(t *testing.T) {
	question := "Any recurring characters in this batch? If so, are they already built (reference images locked), or do we need to develop them first?"
	if !looksLikeQuestion(question) {
		t.Fatal("una pregunta del skill debe detectarse")
	}
	prompt := "0-1.2s: medium shot of @Image5, static camera. Visible cut transition.\n1.2-2s: close-up of @Image1."
	if looksLikeQuestion(prompt) {
		t.Fatal("un prompt con timecodes no es una pregunta")
	}
	if looksLikeQuestion(prompt + "\nIs she smiling?") {
		t.Fatal("un prompt con timecodes no debe marcarse como pregunta")
	}
}

func TestBuildRecreateVideoPrompt(t *testing.T) {
	in := recreateInput{
		Images: []recreateImage{{FileID: "i1", Role: "product"}, {FileID: "i2", Role: "person"}},
		Audios: []recreateAudio{{FileID: "a1"}},
	}
	target := ComposeTarget{
		Model: "m", MaxDuration: 15, Durations: []int{5, 10, 15},
		Ratios: []string{"16:9", "9:16"}, Resolutions: []string{"720p", "1080p"}, SupportsAudio: true,
	}
	vp, err := buildRecreateVideoPrompt("prompt", in, target, 30, "9:16", 2)
	if err != nil {
		t.Fatalf("buildRecreateVideoPrompt: %v", err)
	}
	if vp.Duration != 15 || vp.Ratio != "9:16" || vp.Resolution != "720p" || !vp.GenerateAudio {
		t.Fatalf("ajustes inesperados: %+v", vp)
	}
	if len(vp.Segments) != 1 || vp.Segments[0].Prompt != "prompt" || vp.Segments[0].Index != 1 {
		t.Fatalf("segmentos inesperados: %+v", vp.Segments)
	}
	if len(vp.Ingredients) != 2 || vp.Ingredients[0].Role != "product" || vp.Ingredients[1].Role != "person" {
		t.Fatalf("ingredientes inesperados: %+v", vp.Ingredients)
	}
	if len(vp.AudioRefs) != 1 || vp.AudioRefs[0].Tag != "@Audio1" {
		t.Fatalf("audios inesperados: %+v", vp.AudioRefs)
	}
	found := false
	for _, w := range vp.Warnings {
		if strings.Contains(w, "30 s") {
			found = true
		}
	}
	if !found {
		t.Fatalf("falta el aviso de duracion: %v", vp.Warnings)
	}
	if _, err := buildRecreateVideoPrompt("  ", in, target, 10, "9:16", 1); err == nil {
		t.Fatal("un prompt vacio debe fallar")
	}
}
