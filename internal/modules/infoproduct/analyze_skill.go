package infoproduct

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// analyzeSkillRaw is the DCS-ANALYZE skill (forensic video breakdown) used as
// the system prompt of step 1. The file is copied verbatim from
// internal/Prompt/Synappta Skill analizador de video.md.
//
//go:embed "prompts/Synappta Skill analizador de video.md"
var analyzeSkillRaw string

const (
	analyzeSkillName            = "DCS-ANALYZE"
	analyzeSkillEnv             = "VIDEO_ANALYZER_SKILL"
	analyzeSkillLegacy          = "legacy"
	analyzeModelEnv             = "LLM_MODEL_ANALYZE_VIDEO"
	analyzeSamplingFPSEnv       = "VIDEO_ANALYZE_FPS"
	analyzeLegacyMaxTokens      = 8192
	analyzeMaxTokens            = 32768
	analyzeTemperature          = 0.2
	analyzeCallTimeout          = 8 * time.Minute
	analyzeDetailLevel          = "forensic"
	analyzeDefaultMaxShots      = 120
	analyzeMinSamplingFPS       = 1
	analyzeMaxSamplingFPS       = 24
	analyzeTimelineTolerance    = 0.011
	analyzeDurationTolerance    = 0.05
	analyzeTransportSamplingFPS = 1
	// analyzePipelineVersion invalidates analyses produced before a fix (for
	// example the video-first order that made the model invent the content).
	analyzePipelineVersion = 2
	// analyzeDefaultModel is used when the caller sends no model (the front's
	// "Automatico" option). Older text models ignore most of the skill contract,
	// so the step-1 default favours a video-capable, schema-complete model.
	analyzeDefaultModel = "google/gemini-3.8-flash"
	// analyzeCutAlignmentTolerance is how far a shot boundary may sit from an
	// ffmpeg-measured cut before we consider the cut missing.
	analyzeCutAlignmentTolerance = 0.25
)

// parseSkillDocument removes a leading UTF-8 BOM, normalises CRLF and strips a
// YAML frontmatter block, returning its name and the body.
func parseSkillDocument(raw string) (name, body string, err error) {
	raw = strings.TrimSpace(normalizeNewlines(stripBOM(raw)))
	if raw == "" {
		return "", "", errors.New("el skill esta vacio")
	}
	if !strings.HasPrefix(raw, "---\n") && raw != "---" {
		name, body = frontmatterName(raw), raw
	} else {
		lines := strings.Split(raw, "\n")
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end < 0 {
			return "", "", errors.New("el frontmatter del skill no tiene cierre")
		}
		name = frontmatterName(strings.Join(lines[1:end], "\n"))
		body = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	}
	if name == "" {
		return "", "", errors.New("el skill no declara name en el frontmatter")
	}
	if body == "" {
		return "", "", errors.New("el skill quedo vacio tras quitar el frontmatter")
	}
	return name, body, nil
}

func loadAnalyzeSkill(raw string) (string, error) {
	name, body, err := parseSkillDocument(raw)
	if err != nil {
		return "", err
	}
	if name != analyzeSkillName {
		return "", fmt.Errorf("el skill declara name=%q y se esperaba %q", name, analyzeSkillName)
	}
	return body, nil
}

// LoadVideoAnalyzerSystemPrompt returns the embedded DCS-ANALYZE skill without
// its YAML frontmatter. The skill is already non-interactive.
func LoadVideoAnalyzerSystemPrompt() (string, error) {
	return loadAnalyzeSkill(analyzeSkillRaw)
}

func init() {
	sum := sha256.Sum256([]byte(analyzeSkillRaw))
	short := fmt.Sprintf("%x", sum)
	if len(short) > 8 {
		short = short[:8]
	}
	name, _, perr := parseSkillDocument(analyzeSkillRaw)
	if perr != nil {
		name = ""
	}
	if _, err := LoadVideoAnalyzerSystemPrompt(); err != nil {
		log.Printf("[infoproduct] skill de analisis: name=%q bytes=%d sha256=%s error=%v", name, len(analyzeSkillRaw), short, err)
		return
	}
	log.Printf("[infoproduct] skill de analisis: name=%s bytes=%d sha256=%s", name, len(analyzeSkillRaw), short)
}

// analyzeSkillEnabled reports whether step 1 uses the DCS-ANALYZE skill.
// VIDEO_ANALYZER_SKILL=legacy restores the previous analyzer.
func analyzeSkillEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(analyzeSkillEnv)), analyzeSkillLegacy)
}

// analyzeModel resolves the brain model of this step: LLM_MODEL_ANALYZE_VIDEO
// wins, then the model the front sent, then analyzeDefaultModel.
func analyzeModel(model string) string {
	if v := strings.TrimSpace(os.Getenv(analyzeModelEnv)); v != "" {
		return v
	}
	if m := strings.TrimSpace(model); m != "" {
		return m
	}
	return analyzeDefaultModel
}

// analyzeRequestedSamplingFPS reads VIDEO_ANALYZE_FPS (default 1, clamped to
// 1..24). The OpenRouter transport samples video at 1 fps, so this value is
// declared, never used to resample.
func analyzeRequestedSamplingFPS() int {
	raw := strings.TrimSpace(os.Getenv(analyzeSamplingFPSEnv))
	if raw == "" {
		return analyzeTransportSamplingFPS
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < analyzeMinSamplingFPS || v > analyzeMaxSamplingFPS {
		log.Printf("[infoproduct] %s=%q invalido: se usa %d fps", analyzeSamplingFPSEnv, raw, analyzeTransportSamplingFPS)
		return analyzeTransportSamplingFPS
	}
	return v
}

// effectiveSamplingFPS is what the model really receives: the transport
// ceiling, never a value we cannot honour.
func effectiveSamplingFPS() int {
	return analyzeTransportSamplingFPS
}

// ── DCS-ANALYZE document ─────────────────────────────────────────────────

// analyzeShot is the subset of a skill shot the validator and the legacy
// mapping need.
type analyzeShot struct {
	Index           int     `json:"index"`
	StartS          float64 `json:"start_s"`
	EndS            float64 `json:"end_s"`
	DurationS       float64 `json:"duration_s"`
	MicroShot       bool    `json:"micro_shot"`
	TimingPrecision string  `json:"timing_precision"`
	ShotSize        string  `json:"shot_size"`
	AngleVertical   string  `json:"angle_vertical"`
	CameraMovement  struct {
		Type string `json:"type"`
	} `json:"camera_movement"`
	TransitionOut struct {
		Type string `json:"type"`
	} `json:"transition_out"`
	SubjectAction  string `json:"subject_action"`
	ProductVisible struct {
		Visible  bool   `json:"visible"`
		Piece    string `json:"piece"`
		HowShown string `json:"how_shown"`
	} `json:"product_visible"`
}

type analyzeLegacyShot struct {
	N            int    `json:"n"`
	Tiempo       string `json:"tiempo"`
	PlanoYCamara string `json:"plano_y_camara"`
	Descripcion  string `json:"descripcion"`
}

type analyzeLegacyUI struct {
	Categoria            string              `json:"categoria"`
	DuracionS            float64             `json:"duracion_s"`
	RelacionAspecto      string              `json:"relacion_aspecto"`
	NumeroDeTomas        int                 `json:"numero_de_tomas"`
	ResolucionFPS        string              `json:"resolucion_fps"`
	Resumen              string              `json:"resumen"`
	Ritmo                string              `json:"ritmo"`
	Gancho               string              `json:"gancho"`
	ModeloPersona        string              `json:"modelo_persona"`
	MotionGraphics       string              `json:"motion_graphics"`
	Animacion            string              `json:"animacion"`
	EscenarioIluminacion string              `json:"escenario_iluminacion"`
	ComoSeMuestra        string              `json:"como_se_muestra_el_producto"`
	Musica               string              `json:"musica"`
	VozEnOff             string              `json:"voz_en_off"`
	LlamadoALaAccion     string              `json:"llamado_a_la_accion"`
	Tomas                []analyzeLegacyShot `json:"tomas"`
	TextoEnPantalla      []string            `json:"texto_en_pantalla"`
	DificilDeReplicar    []string            `json:"dificil_de_replicar"`
}

// analyzeDocument is the subset of the skill JSON this package reads.
type analyzeDocument struct {
	SchemaVersion string `json:"schema_version"`
	Video         struct {
		DurationS   float64  `json:"duration_s"`
		AspectRatio string   `json:"aspect_ratio"`
		Resolution  string   `json:"resolution"`
		FPS         *float64 `json:"fps"`
		ShotCount   int      `json:"shot_count"`
		Format      string   `json:"format"`
	} `json:"video"`
	LegacyUI analyzeLegacyUI `json:"legacy_ui"`
	Shots    []analyzeShot   `json:"shots"`
	Talent   struct {
		Present bool `json:"present"`
		People  []struct {
			Role       string `json:"role"`
			Appearance string `json:"appearance"`
		} `json:"people"`
	} `json:"talent"`
	Audio struct {
		Music struct {
			Present bool     `json:"present"`
			Genre   string   `json:"genre"`
			Tempo   *float64 `json:"tempo_bpm_estimate"`
			Mood    string   `json:"mood"`
		} `json:"music"`
		Speech struct {
			Present  bool   `json:"present"`
			Language string `json:"language"`
			Type     string `json:"type"`
			Summary  string `json:"summary"`
		} `json:"speech"`
		SFX []struct {
			Type string `json:"type"`
		} `json:"sfx"`
	} `json:"audio_observations"`
	Graphics struct {
		HasOnScreenText bool `json:"has_on_screen_text"`
		Items           []struct {
			Type         string `json:"type"`
			TextVerbatim string `json:"text_verbatim"`
			TimeRangeS   string `json:"time_range_s"`
			FontStyle    string `json:"font_style"`
			Position     string `json:"position"`
		} `json:"items"`
	} `json:"graphics_summary"`
	Editing struct {
		Metrics struct {
			CutCount           int     `json:"cut_count"`
			MicroShotCount     int     `json:"micro_shot_count"`
			AverageShotLengthS float64 `json:"average_shot_length_s"`
		} `json:"metrics"`
	} `json:"editing_style"`
	Quality struct {
		ChecksPassed   bool              `json:"checks_passed"`
		Uncertainties  []string          `json:"uncertainties"`
		Assumptions    []string          `json:"assumptions"`
		TimelineSource string            `json:"timeline_source"`
		ModelShots     int               `json:"model_shots"`
		ModelShotsList []json.RawMessage `json:"model_shots_list"`
	} `json:"quality"`
}

// analyzeLegacy mirrors the breakdown contract the current front already reads.
type analyzeLegacy struct {
	Summary             string          `json:"summary"`
	VideoType           string          `json:"video_type"`
	VideoTypeNotes      string          `json:"video_type_notes,omitempty"`
	DurationSec         float64         `json:"duration_sec"`
	AspectRatio         string          `json:"aspect_ratio"`
	Pacing              string          `json:"pacing"`
	Hook                string          `json:"hook"`
	CutsCount           int             `json:"cuts_count"`
	Shots               []legacyShot    `json:"shots"`
	HasPerson           bool            `json:"has_person"`
	Person              legacyPerson    `json:"person"`
	HasMotionGraphics   bool            `json:"has_motion_graphics"`
	MotionGraphics      string          `json:"motion_graphics"`
	OnScreenText        []legacyText    `json:"on_screen_text"`
	AnimationStyle      string          `json:"animation_style"`
	SettingAndLighting  string          `json:"setting_and_lighting"`
	ProductPresentation string          `json:"product_presentation"`
	Music               legacyMusic     `json:"music"`
	Voiceover           legacyVoiceover `json:"voiceover"`
	SoundEffects        []string        `json:"sound_effects"`
	CallToAction        string          `json:"call_to_action"`
	ReplicationNotes    []string        `json:"replication_notes"`
	SchemaVersion       string          `json:"schema_version"`
	LegacyUI            json.RawMessage `json:"legacy_ui,omitempty"`
	DCSAnalyze          json.RawMessage `json:"dcs_analyze"`
}

type legacyShot struct {
	Index          int     `json:"index"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	ShotType       string  `json:"shot_type,omitempty"`
	CameraAngle    string  `json:"camera_angle,omitempty"`
	CameraMovement string  `json:"camera_movement,omitempty"`
	Description    string  `json:"description,omitempty"`
	TransitionOut  string  `json:"transition_out,omitempty"`
}

type legacyText struct {
	Time  string `json:"time,omitempty"`
	Text  string `json:"text,omitempty"`
	Style string `json:"style,omitempty"`
}

type legacyPerson struct {
	Description string `json:"description,omitempty"`
	Actions     string `json:"actions,omitempty"`
}

type legacyMusic struct {
	Present     bool    `json:"present"`
	Genre       string  `json:"genre,omitempty"`
	Mood        string  `json:"mood,omitempty"`
	Tempo       string  `json:"tempo,omitempty"`
	Description string  `json:"description,omitempty"`
	BPM         float64 `json:"bpm,omitempty"`
}

type legacyVoiceover struct {
	Present    bool   `json:"present"`
	Language   string `json:"language,omitempty"`
	Tone       string `json:"tone,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

// ── Validation (pure) ────────────────────────────────────────────────────

func sameTime(a, b, tolerance float64) bool {
	return absf(a-b) <= tolerance
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func fmtSeconds(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// missingMeasuredCuts returns the ffmpeg-measured cut times that have no shot
// boundary within the alignment tolerance.
func missingMeasuredCuts(doc *analyzeDocument, measuredCuts []float64) []float64 {
	var missing []float64
	for _, cut := range measuredCuts {
		found := false
		for i := 1; i < len(doc.Shots); i++ {
			if sameTime(doc.Shots[i].StartS, cut, analyzeCutAlignmentTolerance) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, cut)
		}
	}
	return missing
}

// requiredAnalyzeSections are the top-level blocks the technical sheet needs.
// Weaker models return a trimmed answer (just summary + shots) and the front
// would show nothing technical, so a missing block is a contract violation.
var requiredAnalyzeSections = []string{
	"schema_version", "video", "legacy_ui", "shots",
	"visual_style", "editing_style", "camera_summary",
	"technical_signature", "effects_summary", "transitions_summary",
	"graphics_summary", "hook_and_structure",
}

// missingAnalyzeSections lists the required blocks that are absent or empty.
func missingAnalyzeSections(raw json.RawMessage) []string {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return append([]string(nil), requiredAnalyzeSections...)
	}
	missing := make([]string, 0, len(requiredAnalyzeSections))
	for _, key := range requiredAnalyzeSections {
		value, ok := doc[key]
		trimmed := bytes.TrimSpace(value)
		if !ok || len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" || string(trimmed) == "[]" {
			missing = append(missing, key)
		}
	}
	return missing
}

// validateAnalyzeDocument checks the consistency rules of the skill output.
// errors are hard inconsistencies (worth a retry); warnings are quality
// issues that never block the analysis. measuredCuts are the cut times ffmpeg
// detected on the reference; with 1 fps sampling they are the only reliable
// way to notice an under-segmented shot list.
func validateAnalyzeDocument(doc *analyzeDocument, measuredCuts []float64) ([]string, []string) {
	var errs, warns []string
	if doc == nil {
		return []string{"la respuesta no es un objeto JSON del skill"}, nil
	}
	if strings.TrimSpace(doc.SchemaVersion) == "" {
		errs = append(errs, "falta schema_version")
	}
	if len(doc.Shots) == 0 {
		errs = append(errs, "shots[] esta vacio")
		return errs, warns
	}
	if !sameTime(doc.Shots[0].StartS, 0, analyzeTimelineTolerance) {
		errs = append(errs, fmt.Sprintf("shots[0].start_s=%s y debe ser 0.00", fmtSeconds(doc.Shots[0].StartS)))
	}
	micro := 0
	for i, shot := range doc.Shots {
		if shot.MicroShot || shot.EndS-shot.StartS < 0.30 {
			micro++
		}
		if !sameTime(shot.DurationS, shot.EndS-shot.StartS, analyzeTimelineTolerance) {
			errs = append(errs, fmt.Sprintf("shots[%d].duration_s=%s no coincide con end_s-start_s=%s",
				i, fmtSeconds(shot.DurationS), fmtSeconds(shot.EndS-shot.StartS)))
		}
		if shot.EndS < shot.StartS {
			errs = append(errs, fmt.Sprintf("shots[%d] termina (%s) antes de empezar (%s)", i, fmtSeconds(shot.EndS), fmtSeconds(shot.StartS)))
		}
		if i > 0 && !sameTime(shot.StartS, doc.Shots[i-1].EndS, analyzeTimelineTolerance) {
			errs = append(errs, fmt.Sprintf("shots[%d].start_s=%s no coincide con shots[%d].end_s=%s",
				i, fmtSeconds(shot.StartS), i-1, fmtSeconds(doc.Shots[i-1].EndS)))
		}
		if strings.TrimSpace(shot.ShotSize) == "" {
			warns = append(warns, fmt.Sprintf("shots[%d] sin shot_size", i))
		}
		if strings.TrimSpace(shot.AngleVertical) == "" {
			warns = append(warns, fmt.Sprintf("shots[%d] sin angle_vertical", i))
		}
		if strings.TrimSpace(shot.CameraMovement.Type) == "" {
			warns = append(warns, fmt.Sprintf("shots[%d] sin camera_movement.type", i))
		}
		if i < len(doc.Shots)-1 && strings.TrimSpace(shot.TransitionOut.Type) == "" {
			warns = append(warns, fmt.Sprintf("shots[%d] sin transition_out", i))
		}
	}
	last := doc.Shots[len(doc.Shots)-1]
	if doc.Video.DurationS > 0 && !sameTime(last.EndS, doc.Video.DurationS, analyzeDurationTolerance) {
		errs = append(errs, fmt.Sprintf("shots[%d].end_s=%s no coincide con video.duration_s=%s",
			len(doc.Shots)-1, fmtSeconds(last.EndS), fmtSeconds(doc.Video.DurationS)))
	}
	n := len(doc.Shots)
	if doc.Video.ShotCount != n {
		errs = append(errs, fmt.Sprintf("video.shot_count=%d y shots[] tiene %d", doc.Video.ShotCount, n))
	}
	if doc.LegacyUI.NumeroDeTomas != n {
		errs = append(errs, fmt.Sprintf("legacy_ui.numero_de_tomas=%d y shots[] tiene %d", doc.LegacyUI.NumeroDeTomas, n))
	}
	if len(doc.LegacyUI.Tomas) != n {
		errs = append(errs, fmt.Sprintf("legacy_ui.tomas tiene %d entradas y shots[] tiene %d", len(doc.LegacyUI.Tomas), n))
	}
	if doc.Editing.Metrics.MicroShotCount != 0 && micro > 0 && doc.Editing.Metrics.MicroShotCount != micro {
		warns = append(warns, fmt.Sprintf("editing_style.metrics.micro_shot_count=%d y se contaron %d tomas < 0.30 s",
			doc.Editing.Metrics.MicroShotCount, micro))
	}
	if missing := missingMeasuredCuts(doc, measuredCuts); len(missing) > 0 {
		msg := fmt.Sprintf("faltan %d de %d cortes medidos por ffmpeg (muestreo a %d fps): %s",
			len(missing), len(measuredCuts), effectiveSamplingFPS(), formatCutList(missing))
		if len(missing)*2 > len(measuredCuts) {
			errs = append(errs, msg)
		} else {
			warns = append(warns, msg)
		}
	}
	return errs, warns
}

// formatCutList renders cut times for logs and retry prompts.
func formatCutList(cuts []float64) string {
	parts := make([]string, 0, len(cuts))
	for _, c := range cuts {
		parts = append(parts, fmtSeconds(c))
	}
	return strings.Join(parts, ", ")
}

// ── Legacy mapping ───────────────────────────────────────────────────────

func legacyShotsFromAnalyze(doc *analyzeDocument) []legacyShot {
	out := make([]legacyShot, 0, len(doc.Shots))
	for i, shot := range doc.Shots {
		desc := strings.TrimSpace(shot.SubjectAction)
		if i < len(doc.LegacyUI.Tomas) {
			if v := strings.TrimSpace(doc.LegacyUI.Tomas[i].Descripcion); v != "" {
				desc = v
			}
		}
		out = append(out, legacyShot{
			Index:          i + 1,
			Start:          shot.StartS,
			End:            shot.EndS,
			ShotType:       shot.ShotSize,
			CameraAngle:    shot.AngleVertical,
			CameraMovement: shot.CameraMovement.Type,
			Description:    desc,
			TransitionOut:  shot.TransitionOut.Type,
		})
	}
	return out
}

func legacyTextsFromAnalyze(doc *analyzeDocument) []legacyText {
	if len(doc.Graphics.Items) > 0 {
		out := make([]legacyText, 0, len(doc.Graphics.Items))
		for _, item := range doc.Graphics.Items {
			style := strings.TrimSpace(strings.Join(compactStrings(item.FontStyle, item.Position), " · "))
			out = append(out, legacyText{
				Time:  item.TimeRangeS,
				Text:  item.TextVerbatim,
				Style: style,
			})
		}
		return out
	}
	out := make([]legacyText, 0, len(doc.LegacyUI.TextoEnPantalla))
	for _, line := range doc.LegacyUI.TextoEnPantalla {
		out = append(out, legacyText{Text: line})
	}
	return out
}

func compactStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func legacySoundEffects(doc *analyzeDocument) []string {
	out := make([]string, 0, len(doc.Audio.SFX))
	for _, sfx := range doc.Audio.SFX {
		if s := strings.TrimSpace(sfx.Type); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// mergeAnalyzeDocument builds the stored breakdown: the exact field names the
// current front already reads (derived from legacy_ui/shot list) plus the full
// skill JSON under dcs_analyze.
func mergeAnalyzeDocument(doc *analyzeDocument, dcsRaw json.RawMessage) (json.RawMessage, error) {
	if doc == nil {
		return nil, errors.New("no hay analisis del skill para mapear")
	}
	duration := doc.Video.DurationS
	if duration <= 0 {
		duration = doc.LegacyUI.DuracionS
	}
	ratio := firstNonEmpty(doc.Video.AspectRatio, doc.LegacyUI.RelacionAspecto)
	shots := legacyShotsFromAnalyze(doc)
	cuts := doc.Editing.Metrics.CutCount
	if cuts <= 0 && len(shots) > 1 {
		cuts = len(shots) - 1
	}
	talent := ""
	for _, p := range doc.Talent.People {
		if s := strings.TrimSpace(p.Appearance); s != "" {
			talent = s
			break
		}
	}
	if talent == "" {
		talent = doc.LegacyUI.ModeloPersona
	}
	music := legacyMusic{
		Present:     doc.Audio.Music.Present,
		Genre:       doc.Audio.Music.Genre,
		Mood:        doc.Audio.Music.Mood,
		Description: doc.LegacyUI.Musica,
	}
	if doc.Audio.Music.Tempo != nil {
		music.BPM = *doc.Audio.Music.Tempo
	}
	voice := legacyVoiceover{
		Present:    doc.Audio.Speech.Present,
		Language:   doc.Audio.Speech.Language,
		Transcript: firstNonEmpty(doc.Audio.Speech.Summary, doc.LegacyUI.VozEnOff),
	}
	legacyUI, err := json.Marshal(doc.LegacyUI)
	if err != nil {
		legacyUI = nil
	}
	out := analyzeLegacy{
		Summary:             firstNonEmpty(doc.LegacyUI.Resumen, doc.LegacyUI.Gancho),
		VideoType:           firstNonEmpty(doc.LegacyUI.Categoria, doc.Video.Format),
		DurationSec:         duration,
		AspectRatio:         ratio,
		Pacing:              doc.LegacyUI.Ritmo,
		Hook:                doc.LegacyUI.Gancho,
		CutsCount:           cuts,
		Shots:               shots,
		HasPerson:           doc.Talent.Present,
		Person:              legacyPerson{Description: talent},
		HasMotionGraphics:   doc.Graphics.HasOnScreenText || len(doc.Graphics.Items) > 0,
		MotionGraphics:      doc.LegacyUI.MotionGraphics,
		OnScreenText:        legacyTextsFromAnalyze(doc),
		AnimationStyle:      doc.LegacyUI.Animacion,
		SettingAndLighting:  doc.LegacyUI.EscenarioIluminacion,
		ProductPresentation: doc.LegacyUI.ComoSeMuestra,
		Music:               music,
		Voiceover:           voice,
		SoundEffects:        legacySoundEffects(doc),
		CallToAction:        doc.LegacyUI.LlamadoALaAccion,
		ReplicationNotes:    doc.LegacyUI.DificilDeReplicar,
		SchemaVersion:       doc.SchemaVersion,
		LegacyUI:            legacyUI,
		DCSAnalyze:          dcsRaw,
	}
	merged, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// markChecksFailed flips quality.checks_passed inside the raw skill JSON so the
// stored document records that the consistency checks did not pass.
func markChecksFailed(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	quality, ok := m["quality"].(map[string]any)
	if !ok {
		quality = map[string]any{}
	}
	quality["checks_passed"] = false
	m["quality"] = quality
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// markPipelineVersion stamps the analyzer revision inside quality, so a cached
// analysis produced by an older (buggy) pipeline can be detected and redone.
func markPipelineVersion(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	quality, ok := m["quality"].(map[string]any)
	if !ok {
		quality = map[string]any{}
	}
	quality["pipeline_version"] = analyzePipelineVersion
	m["quality"] = quality
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// analysisPipelineVersion reads quality.pipeline_version from a stored skill
// document (0 when absent).
func analysisPipelineVersion(raw json.RawMessage) int {
	var probe struct {
		Quality struct {
			PipelineVersion float64 `json:"pipeline_version"`
		} `json:"quality"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0
	}
	return int(probe.Quality.PipelineVersion)
}

// ── Call orchestration ───────────────────────────────────────────────────

// analyzePayload is the user turn: measured params first, then the video.
func analyzePayload(meta MediaMeta, video []byte) []map[string]any {
	params := map[string]any{
		"duration_s":   meta.DurationSec,
		"aspect_ratio": meta.AspectRatio,
		"resolution":   fmt.Sprintf("%dx%d", meta.Width, meta.Height),
		"fps":          meta.FPS,
		"sampling_fps": effectiveSamplingFPS(),
		"detail_level": analyzeDetailLevel,
		"max_shots":    analyzeDefaultMaxShots,
	}
	// ffmpeg-measured cuts are the only reliable boundary evidence when the
	// transport samples at 1 fps: without them short cuts get merged.
	if len(meta.Cuts) > 0 {
		params["measured_cuts_s"] = meta.Cuts
		params["measured_shot_count"] = meta.CutsCount
	}
	body, _ := json.Marshal(params)
	intro := "VIDEO PARAMETERS (measured by ffprobe/ffmpeg, ground truth):\n" + string(body) +
		"\n\nThe attached video is the reference. Watch it, listen to its audio and reply with the single JSON object required by the system prompt."
	if len(meta.Cuts) > 0 {
		intro += fmt.Sprintf("\n\nMANDATORY SEGMENTATION — ffmpeg measured %d cuts in this file, so it has %d shots.\n"+
			"Cut times in seconds: %s.\n"+
			"Your shots[] MUST contain a boundary at every one of those times (±0.05 s) and shots.length MUST be %d; add more only if you clearly see a cut ffmpeg missed. "+
			"Sampling is %d fps, so you cannot see every frame: place those boundaries from the measured times plus audio and rhythm evidence, and mark them timing_precision=\"approx\" when unsure. "+
			"Never merge two measured cuts into one shot. video.shot_count, editing_style.metrics.shot_count, "+
			"legacy_ui.numero_de_tomas and legacy_ui.tomas.length must all equal shots.length.",
			len(meta.Cuts), meta.CutsCount, formatCutList(meta.Cuts), meta.CutsCount, effectiveSamplingFPS())
	}
	// The text goes first and the video last: OpenRouter's Gemini bridge only
	// forwards the video part when it follows the text (same order the legacy
	// analyzer used). With the video first the model receives no media at all
	// and invents the content.
	return []map[string]any{
		textPart(intro),
		videoPart("data:video/mp4;base64," + base64.StdEncoding.EncodeToString(video)),
	}
}

// analyzeRepairPrompt asks for the complete corrected JSON, listing the exact
// consistency errors found in the previous answer.
func analyzeRepairPrompt(previous json.RawMessage, errs []string) string {
	var b strings.Builder
	b.WriteString("Your previous answer failed these consistency checks:\n")
	for _, e := range errs {
		b.WriteString("- ")
		b.WriteString(e)
		b.WriteString("\n")
	}
	b.WriteString("\nPrevious answer:\n")
	b.Write(previous)
	b.WriteString("\n\nReturn the COMPLETE corrected JSON object again, fixing every listed issue. No commentary, no markdown fences.")
	return b.String()
}

type analyzeAttempt struct {
	raw    json.RawMessage
	doc    *analyzeDocument
	errors []string
	warns  []string
}

func parseAnalyzeAnswer(text string, measuredCuts []float64) analyzeAttempt {
	raw, err := extractJSON(text)
	if err != nil {
		return analyzeAttempt{errors: []string{err.Error()}}
	}
	doc := &analyzeDocument{}
	if err := json.Unmarshal(raw, doc); err != nil {
		return analyzeAttempt{raw: raw, errors: []string{"el JSON del skill no tiene la forma esperada: " + err.Error()}}
	}
	errs, warns := validateAnalyzeDocument(doc, measuredCuts)
	if missing := missingAnalyzeSections(raw); len(missing) > 0 {
		errs = append(errs, "faltan secciones del contrato: "+strings.Join(missing, ", "))
	}
	return analyzeAttempt{raw: raw, doc: doc, errors: errs, warns: warns}
}

func addUsage(u *usage, extra usage) {
	u.PromptTokens += extra.PromptTokens
	u.CompletionTokens += extra.CompletionTokens
	u.CostUSD += extra.CostUSD
}

func (m *Module) analyzeCall(ctx context.Context, baseURL, apiKey, model, system string, parts []map[string]any) (string, usage, error) {
	return m.brain.chatWithOptions(ctx, baseURL, apiKey, model, system, parts, analyzeMaxTokens, chatOptions{
		temperature: analyzeTemperature,
		timeout:     analyzeCallTimeout,
		jsonMode:    true,
	})
}

// analyzeWithSkill runs step 1 with the DCS-ANALYZE skill and returns the
// breakdown ready to store (legacy field names + full skill JSON).
func (m *Module) analyzeWithSkill(ctx context.Context, baseURL, apiKey, model string, meta MediaMeta, video []byte) (json.RawMessage, usage, error) {
	system, err := LoadVideoAnalyzerSystemPrompt()
	if err != nil {
		return nil, usage{}, err
	}
	model = analyzeModel(model)
	log.Printf("[infoproduct] analyze rama=%s model=%s system_bytes=%d sampling_fps=%d video_bytes=%d",
		analyzeSkillName, normalizeModel(model), len(system), effectiveSamplingFPS(), len(video))
	if requested := analyzeRequestedSamplingFPS(); requested != effectiveSamplingFPS() {
		log.Printf("[infoproduct] analyze: %s=%d no aplica (el transporte OpenRouter muestrea a %d fps)",
			analyzeSamplingFPSEnv, requested, effectiveSamplingFPS())
	}

	started := time.Now()
	parts := analyzePayload(meta, video)
	answer, u, err := m.analyzeCall(ctx, baseURL, apiKey, model, system, parts)
	if err != nil {
		return nil, u, err
	}
	attempt := parseAnalyzeAnswer(answer, meta.Cuts)
	if len(attempt.errors) > 0 {
		log.Printf("[infoproduct] analyze: JSON inconsistente (%d), reintento unico: %s",
			len(attempt.errors), strings.Join(attempt.errors, "; "))
		retryParts := []map[string]any{textPart(analyzeRepairPrompt(attempt.raw, attempt.errors))}
		retrySystem := system + "\n\nREMINDER: your previous answer failed the JSON consistency checks. Return the complete corrected JSON object now."
		if second, u2, rerr := m.analyzeCall(ctx, baseURL, apiKey, model, retrySystem, retryParts); rerr == nil {
			addUsage(&u, u2)
			fixed := parseAnalyzeAnswer(second, meta.Cuts)
			if fixed.doc != nil && len(fixed.errors) <= len(attempt.errors) {
				attempt = fixed
			}
		} else {
			log.Printf("[infoproduct] analyze: el reintento fallo (%v)", rerr)
		}
	}
	// The model cannot see every cut at 1 fps: rebuild the timeline from the
	// ffmpeg measurements and keep the model's own shot list in quality.
	if attempt.doc != nil && len(meta.Cuts) > 0 {
		if reconciled, changed := reconcileTimeline(attempt.raw, meta.Cuts); changed {
			var doc analyzeDocument
			if json.Unmarshal(reconciled, &doc) == nil {
				attempt.raw, attempt.doc = reconciled, &doc
				attempt.errors, attempt.warns = validateAnalyzeDocument(attempt.doc, meta.Cuts)
				log.Printf("[infoproduct] analyze: linea de tiempo reconciliada con %d cortes medidos (%d tomas del modelo -> %d tomas)",
					len(meta.Cuts), len(doc.Quality.ModelShotsList), len(attempt.doc.Shots))
			}
		}
	}
	checksPassed := len(attempt.errors) == 0
	for _, w := range attempt.warns {
		log.Printf("[infoproduct] analyze aviso: %s", w)
	}
	if !checksPassed {
		log.Printf("[infoproduct] analyze: se guarda con quality.checks_passed=false: %s", strings.Join(attempt.errors, "; "))
		attempt.raw = markChecksFailed(attempt.raw)
		var doc analyzeDocument
		if json.Unmarshal(attempt.raw, &doc) == nil {
			attempt.doc = &doc
		}
	}
	attempt.raw = markPipelineVersion(attempt.raw)
	merged, err := mergeAnalyzeDocument(attempt.doc, attempt.raw)
	if err != nil {
		return nil, u, err
	}
	log.Printf("[infoproduct] analyze listo en %s: shots=%d tokens_in=%d tokens_out=%d checks_passed=%t",
		time.Since(started).Round(time.Millisecond), len(attempt.doc.Shots), u.PromptTokens, u.CompletionTokens, checksPassed)
	return merged, u, nil
}
