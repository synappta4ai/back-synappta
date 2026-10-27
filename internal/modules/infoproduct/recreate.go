package infoproduct

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"regexp"
	"strings"
)

//go:embed "prompts/Synappta Skill actuacion Infoproducto.md"
var recreateSkillRaw string

const (
	defaultRecreateMaxDuration = 15
	defaultRecreateMinShot     = 0.30
	defaultRecreateMaxWords    = 700
	recreateMaxTokens          = 2500
	recreateTemperature        = 0.35
	recreateSkillName          = "DCS-RECREATE"
)

// recreateSkillMeta is the startup fingerprint of the embedded skill.
type recreateSkillMeta struct {
	Name   string
	Bytes  int
	SHA256 string
}

func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}

func normalizeNewlines(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func frontmatterName(front string) string {
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "name:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		if i := strings.Index(v, " description:"); i >= 0 {
			v = v[:i]
		}
		return strings.Trim(v, `"'`)
	}
	return ""
}

// parseRecreateSkill removes a leading UTF-8 BOM, normalises CRLF and returns
// the frontmatter name plus the body (frontmatter stripped).
func parseRecreateSkill(raw string) (name, body string, err error) {
	raw = strings.TrimSpace(normalizeNewlines(stripBOM(raw)))
	if raw == "" {
		return "", "", errors.New("el skill de recreacion esta vacio")
	}
	if strings.HasPrefix(raw, "---\n") || raw == "---" {
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
	} else {
		name = frontmatterName(raw)
		body = raw
	}
	if name == "" {
		return "", "", errors.New("el skill no declara name en el frontmatter")
	}
	if body == "" {
		return "", "", errors.New("el skill quedo vacio tras quitar el frontmatter")
	}
	return name, body, nil
}

func loadRecreateSkill(raw string) (string, error) {
	name, body, err := parseRecreateSkill(raw)
	if err != nil {
		return "", err
	}
	if name != recreateSkillName {
		return "", fmt.Errorf("el skill declara name=%q y se esperaba %q: se usa el flujo anterior", name, recreateSkillName)
	}
	legacyColon := ":"
	if strings.Contains(body, "Camera Capture"+legacyColon) {
		return "", errors.New("el skill contiene la marca de la version anterior (Camera Capture): se usa el flujo anterior")
	}
	return body, nil
}

// LoadRecreateSystemPrompt returns the embedded DCS-RECREATE skill without its
// YAML frontmatter. The skill is already non-interactive.
func LoadRecreateSystemPrompt() (string, error) {
	return loadRecreateSkill(recreateSkillRaw)
}

func inspectRecreateSkill(raw string) recreateSkillMeta {
	sum := sha256.Sum256([]byte(raw))
	name, _, _ := parseRecreateSkill(raw)
	return recreateSkillMeta{Name: name, Bytes: len(raw), SHA256: fmt.Sprintf("%x", sum)}
}

func init() {
	meta := inspectRecreateSkill(recreateSkillRaw)
	short := meta.SHA256
	if len(short) > 8 {
		short = short[:8]
	}
	if _, err := LoadRecreateSystemPrompt(); err != nil {
		log.Printf("[infoproduct] skill de recreacion: name=%q bytes=%d sha256=%s error=%v", meta.Name, meta.Bytes, short, err)
		return
	}
	log.Printf("[infoproduct] skill de recreacion: name=%s bytes=%d sha256=%s", meta.Name, meta.Bytes, short)
}

// recreateSkillEnabled reports whether the new single-prompt flow is active.
// VIDEO_RECREATE_SKILL=legacy restores the previous compose behavior.
func recreateSkillEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("VIDEO_RECREATE_SKILL")), "legacy")
}

// recreateModel lets LLM_MODEL_RECREATE_PROMPT override the brain model only
// for this step; empty means "use the model the front sent".
func recreateModel(model string) string {
	if v := strings.TrimSpace(os.Getenv("LLM_MODEL_RECREATE_PROMPT")); v != "" {
		return v
	}
	return model
}

type recreateImage struct {
	FileID string
	Role   string
	Name   string
}

type recreateAudio struct {
	FileID string
	Name   string
}

type recreateInput struct {
	Breakdown     json.RawMessage
	Images        []recreateImage
	Audios        []recreateAudio
	Notes         string
	AspectRatio   string
	FPS           float64
	TotalDuration float64
	MaxDuration   int
	TargetModel   string
}

type recreateShot struct {
	Index          int     `json:"index,omitempty"`
	Start          float64 `json:"start,omitempty"`
	End            float64 `json:"end,omitempty"`
	ShotType       string  `json:"shot_type,omitempty"`
	CameraAngle    string  `json:"camera_angle,omitempty"`
	CameraMovement string  `json:"camera_movement,omitempty"`
	Description    string  `json:"description,omitempty"`
	TransitionOut  string  `json:"transition_out,omitempty"`
}

type recreateBreakdown struct {
	Summary            string         `json:"summary,omitempty"`
	Pacing             string         `json:"pacing,omitempty"`
	SettingAndLighting string         `json:"setting_and_lighting,omitempty"`
	Shots              []recreateShot `json:"shots,omitempty"`
}

// sanitizeBreakdown keeps only what the recreate skill needs: summary, pacing,
// setting/lighting and the shot table. Plain text is returned untouched.
func sanitizeBreakdown(raw json.RawMessage) string {
	var b recreateBreakdown
	if err := json.Unmarshal(raw, &b); err != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

func normalizeIngredientRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "person", "identity", "character", "model":
		return "person"
	case "product", "prop":
		return "product"
	case "environment", "location":
		return "environment"
	case "video":
		return "video"
	case "audio":
		return "audio"
	}
	return ""
}

func nameSuffix(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

func buildIngredientManifest(in recreateInput) []string {
	lines := make([]string, 0, len(in.Images)+len(in.Audios))
	for i, img := range in.Images {
		tag := "@Image" + itoa(i+1)
		switch normalizeIngredientRole(img.Role) {
		case "person":
			lines = append(lines, tag+" = identity reference: the on-screen person"+nameSuffix(img.Name))
		case "environment":
			lines = append(lines, tag+" = environment reference: location and lighting"+nameSuffix(img.Name))
		case "video":
			lines = append(lines, tag+" = motion reference: camera and movement"+nameSuffix(img.Name))
		default:
			lines = append(lines, tag+" = product: the user's product"+nameSuffix(img.Name))
		}
	}
	for i, a := range in.Audios {
		lines = append(lines, "@Audio"+itoa(i+1)+" = soundtrack / rhythm reference"+nameSuffix(a.Name))
	}
	return lines
}

// buildRecreateUserMessage assembles the user turn: INGREDIENTS, BREAKDOWN,
// optional USER_NOTES and PARAMS, in that order.
func buildRecreateUserMessage(in recreateInput) (string, error) {
	manifest := buildIngredientManifest(in)
	if len(manifest) == 0 {
		return "", errors.New("no hay ingredientes para el prompt de recreacion")
	}
	var b strings.Builder
	b.WriteString("INGREDIENTS:\n")
	for i, line := range manifest {
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}
	b.WriteString("\nBREAKDOWN:\n")
	b.WriteString(sanitizeBreakdown(in.Breakdown))
	if notes := strings.TrimSpace(in.Notes); notes != "" {
		b.WriteString("\n\nUSER_NOTES:\n")
		b.WriteString(notes)
	}
	ratio := strings.TrimSpace(in.AspectRatio)
	if ratio == "" {
		ratio = "9:16"
	}
	fps := in.FPS
	if fps <= 0 {
		fps = 24
	}
	maxDuration := in.MaxDuration
	if maxDuration <= 0 {
		maxDuration = defaultRecreateMaxDuration
	}
	b.WriteString("\n\nPARAMS:\n")
	fmt.Fprintf(&b, "aspect_ratio: %s\n", ratio)
	fmt.Fprintf(&b, "fps: %g\n", fps)
	fmt.Fprintf(&b, "total_duration_s: %g\n", in.TotalDuration)
	fmt.Fprintf(&b, "max_duration_s: %d\n", maxDuration)
	fmt.Fprintf(&b, "min_shot_s: %.2f\n", defaultRecreateMinShot)
	fmt.Fprintf(&b, "max_words: %d\n", defaultRecreateMaxWords)
	fmt.Fprintf(&b, "target_model: %s\n", strings.TrimSpace(in.TargetModel))
	return strings.TrimSpace(b.String()), nil
}

var recreatePreamble = regexp.MustCompile(`(?i)^(here('| i)s|below is|aqu[ií] est[aá]|este es)\b.*prompt.*:?\s*$`)

func stripRecreateFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return text
	}
	lines = lines[1:]
	if n := len(lines); n > 0 && strings.HasPrefix(strings.TrimSpace(lines[n-1]), "```") {
		lines = lines[:n-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func stripRecreatePreamble(text string) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 {
		first := strings.TrimSpace(lines[0])
		if first == "" || recreatePreamble.MatchString(first) {
			lines = lines[1:]
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}

// cleanRecreatePrompt removes markdown fences, an obvious preamble and extra
// blank lines. It never rewrites the prompt body.
func cleanRecreatePrompt(text string) (string, error) {
	text = strings.TrimSpace(text)
	text = stripRecreateFence(text)
	text = stripRecreatePreamble(text)
	var out []string
	blank := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 2 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	text = strings.TrimSpace(strings.Join(out, "\n"))
	if text == "" {
		return "", errEmptyPrompt
	}
	return text, nil
}

var recreateTimecode = regexp.MustCompile(`(?m)^\s*\d+(?:\.\d+)?\s*[-\x{2013}]\s*\d`)

// looksLikeQuestion detects a conversational reply (the skill asking for
// missing data) instead of a shot-by-shot prompt.
func looksLikeQuestion(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasSuffix(t, "?") {
		return false
	}
	if recreateTimecode.MatchString(t) {
		return false
	}
	return len(strings.Fields(t)) < 160
}

// buildRecreateVideoPrompt wraps the plain-text prompt in the existing
// VideoPrompt contract so the current front keeps working.
func buildRecreateVideoPrompt(prompt string, in recreateInput, target ComposeTarget, refDuration float64, refRatio string, segmentsWanted int) (*VideoPrompt, error) {
	out := &VideoPrompt{
		Prompt:      prompt,
		Model:       target.Model,
		Ingredients: []Ingredient{},
		AudioRefs:   []AudioRef{},
	}
	out.Duration = pickDuration(int(math.Ceil(refDuration)), target.Durations, target.MaxDuration)
	if out.Duration <= 0 {
		out.Duration = pickDuration(defaultRecreateMaxDuration, target.Durations, target.MaxDuration)
	}
	out.Ratio = pickString("9:16", refRatio, target.Ratios)
	out.Resolution = pickString("", "", target.Resolutions)
	out.GenerateAudio = target.SupportsAudio && len(in.Audios) > 0
	for i, img := range in.Images {
		role := normalizeIngredientRole(img.Role)
		if role == "" {
			role = "product"
		}
		out.Ingredients = append(out.Ingredients, Ingredient{FileID: img.FileID, Tag: "@Image" + itoa(i+1), Role: role})
	}
	for i, a := range in.Audios {
		out.AudioRefs = append(out.AudioRefs, AudioRef{FileID: a.FileID, Tag: "@Audio" + itoa(i+1), Kind: "music"})
	}
	out.Segments = []Segment{{Index: 1, Start: 0, End: float64(out.Duration), Prompt: prompt}}
	if refDuration > 0 && target.MaxDuration > 0 && int(math.Ceil(refDuration)) > target.MaxDuration {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"La referencia dura %.0f s y el modelo %s genera hasta %d s: el prompt cubre el video completo, pero la generacion se limita al maximo del modelo.",
			refDuration, target.Model, target.MaxDuration))
	} else if segmentsWanted > 1 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"La referencia requiere %d generaciones segun el maximo del modelo; este prompt unico se enviara en una sola generacion.",
			segmentsWanted))
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errEmptyPrompt
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
