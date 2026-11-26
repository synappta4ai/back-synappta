// Package writingcopilot powers the "Writing Copilot" agent of VideoMachine:
// it turns a simple idea, an ad script or a short story into cinematographic
// prompts using the embedded Synappta-WRITE skill as the system prompt. The
// flow is stateless: the caller sends the conversation history it wants the
// brain to consider on every turn.
package writingcopilot

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/looks"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/file"
	"synapta/internal/skills"
	"synapta/internal/utils"
)

//go:embed "prompts/Synappta Skill writing copilot.md"
var skillRaw string

// Step-1 structure guides: each video type picks the document that tells the
// brain how to structure the idea before writing the prompts.
//
//go:embed "prompts/master-prompt-publicidad-adaptable.md"
var commercialGuide string

//go:embed "prompts/master-prompt-guion-cinematografico.md"
var narrativeGuide string

// actingEngine is section 6 of the infoproduct acting skill: the performance
// engine (Weston/Mamet, anatomical facts, micro-fidgets, positive locks).
var actingEngine string

// Video types accepted in params.video_type. Empty keeps the base skill only.
const (
	VideoTypeCommercialShort = "commercial_short"
	VideoTypeCommercialLong  = "commercial_long"
	VideoTypeStoryShort      = "story_short"
	VideoTypeStoryLong       = "story_long"
)

const (
	skillName = "Synappta-WRITE"

	envEnabled     = "WRITING_COPILOT_ENABLED"
	envModel       = "LLM_MODEL_WRITING_COPILOT"
	envTemperature = "WRITING_COPILOT_TEMPERATURE"
	envMaxTokens   = "WRITING_COPILOT_MAX_TOKENS"

	// defaultTemperature is the sampling temperature of the agent.
	defaultTemperature = 0.7
	callTimeout        = 6 * time.Minute

	// Completion budget: a pack of N shots needs roughly N*perShot tokens, so
	// the cap grows with the shot count instead of a fixed 8k that truncated
	// 15-shot packs. maxShotsPerPiece is the product limit.
	maxTokensBase    = 8000
	maxTokensPerShot = 2000
	maxShotsPerPiece = 15

	maxHistoryMessages = 20
	maxMessageChars    = 12000
)

// CredentialResolver supplies the tenant credentials per request (same
// pattern as the other modules).
type CredentialResolver func(c *gin.Context) (*credential.Handler, bool)

// FilesResolver supplies the tenant file handler (imagenes de referencia).
type FilesResolver func(c *gin.Context) (*file.Handler, bool)

// Module registers the /writing-copilot routes.
type Module struct {
	creds CredentialResolver
	files FilesResolver
	brain *brain
}

// NewModule builds the module.
func NewModule(creds CredentialResolver, files FilesResolver) *Module {
	return &Module{creds: creds, files: files, brain: newBrain()}
}

// Name implements modules.Module.
func (m *Module) Name() string { return "writing_copilot" }

// Register implements modules.Module.
func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	g := rg.Group("/writing-copilot")
	g.Use(authMw, tenantMw)
	{
		g.GET("/availability", m.Availability)
		g.POST("/chat", m.Chat)
	}
}

// ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ Skill ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬

// parseSkill removes a leading UTF-8 BOM, normalises CRLF and strips a YAML
// frontmatter block, returning its name and the body.
func parseSkill(raw string) (name, body string, err error) {
	raw = strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(raw, "\ufeff"), "\r\n", "\n"))
	if raw == "" {
		return "", "", errors.New("el skill de Writing Copilot esta vacio")
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

// frontmatterName extracts the "name:" value of a YAML frontmatter block.
func frontmatterName(front string) string {
	for _, line := range strings.Split(front, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "name:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		if i := strings.Index(value, " description:"); i >= 0 {
			value = value[:i]
		}
		return strings.Trim(value, `"'`)
	}
	return ""
}

// loadSkill validates the embedded skill and returns the system prompt.
func loadSkill(raw string) (string, error) {
	name, body, err := parseSkill(raw)
	if err != nil {
		return "", err
	}
	if name != skillName {
		return "", fmt.Errorf("el skill declara name=%q y se esperaba %q", name, skillName)
	}
	return body, nil
}

// LoadWritingCopilotSystemPrompt returns the embedded Synappta-WRITE skill
// without its YAML frontmatter.
func LoadWritingCopilotSystemPrompt() (string, error) {
	return loadSkill(skillRaw)
}

// structureGuide returns the step-1 guide for a video type: title, document
// body and the short note that adapts it to the short/long variant.
func structureGuide(videoType string) (title, body, note string, ok bool) {
	switch strings.TrimSpace(videoType) {
	case VideoTypeCommercialShort:
		return "ADAPTIVE COMMERCIAL STORYTELLING (short form)", commercialGuide,
			"Target length: 6 to 30 s. Compress the arc: the hook and the problem may share one action, the solution enters fast and the CTA closes the last shot.", true
	case VideoTypeCommercialLong:
		return "ADAPTIVE COMMERCIAL STORYTELLING (long form)", commercialGuide,
			"Target length: 30 to 60 s. Keep the four phases with room for a before/after contrast and a clear product demonstration.", true
	case VideoTypeStoryShort:
		return "NARRATIVE CORE (short story)", narrativeGuide,
			"Keep one protagonist, one conscious desire and one escalating conflict in a compact three-act skeleton that fits a short piece.", true
	case VideoTypeStoryLong:
		return "NARRATIVE CORE (long story)", narrativeGuide,
			"Develop the full three-act architecture, midpoint, pinch points and character arc exactly as this guide requires.", true
	}
	return "", "", "", false
}

// ValidVideoType reports whether videoType is empty or one of the accepted
// values.
func ValidVideoType(videoType string) bool {
	switch strings.TrimSpace(videoType) {
	case "", VideoTypeCommercialShort, VideoTypeCommercialLong, VideoTypeStoryShort, VideoTypeStoryLong:
		return true
	}
	return false
}

// SystemPromptForVideoType returns the base skill plus the step-1 structure
// guide selected by videoType. An empty or unknown type keeps the base skill.
func SystemPromptForVideoType(videoType string) (string, error) {
	base, err := LoadWritingCopilotSystemPrompt()
	if err != nil {
		return "", err
	}
	title, guide, note, ok := structureGuide(videoType)
	if !ok {
		return withActingEngine(base), nil
	}
	guide = strings.TrimSpace(strings.ReplaceAll(strings.TrimPrefix(guide, "\ufeff"), "\r\n", "\n"))
	section := strings.Join([]string{
		"## 0.5 STEP 1 STRUCTURE GUIDE — " + title,
		"",
		note,
		"",
		"Apply this guide to how you understand, diagnose and structure the idea in STEP 1. Build the beats, the acts and the duration distribution exactly as it requires, then translate that structure into the cinematographic prompts defined in Sections 5 to 9 of this skill. If the guide and this skill disagree, the guide wins for narrative structure, act distribution and timing; this skill wins for shot design, camera, lighting, acting and prompt format.",
		"",
		guide,
	}, "\n")
	return withActingEngine(base + "\n\n" + section), nil
}

// withActingEngine appends the performance engine to any writing-copilot
// system prompt. Every prompt-producing turn uses it, including the final one;
// it enriches acting without changing the story, camera or output format.
func withActingEngine(prompt string) string {
	if actingEngine == "" {
		return prompt
	}
	return prompt + "\n\n## 0.7 ACTING ENGINE — APPLY TO EVERY CHARACTER PERFORMANCE\n\n" +
		"Use this acting engine in every character shot you write, including the final prompt: convert emotions into transitive actions and observable muscular facts, add one asynchronous micro-fidget in acting shots over 1 s, and translate prohibitions into positive locks. It enriches performance only: never change the story, camera, lighting, continuity or output format.\n\n" +
		actingEngine
}

func init() {
	sum := sha256.Sum256([]byte(skillRaw))
	short := fmt.Sprintf("%x", sum)
	if len(short) > 8 {
		short = short[:8]
	}
	name, _, perr := parseSkill(skillRaw)
	if perr != nil {
		name = ""
	}
	if _, err := LoadWritingCopilotSystemPrompt(); err != nil {
		log.Printf("[writingcopilot] skill: name=%q bytes=%d sha256=%s error=%v", name, len(skillRaw), short, err)
	} else {
		log.Printf("[writingcopilot] skill: name=%s bytes=%d sha256=%s", name, len(skillRaw), short)
	}
	if acting, err := skills.ActingEngine(); err != nil {
		log.Printf("[writingcopilot] acting skill: %v", err)
	} else {
		actingEngine = acting
		log.Printf("[writingcopilot] acting skill: bytes=%d", len(acting))
	}
}

// ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ Config ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬

// enabledFromEnv reports the WRITING_COPILOT_ENABLED flag (default true).
func enabledFromEnv() bool {
	raw := strings.TrimSpace(os.Getenv(envEnabled))
	if raw == "" {
		return true
	}
	off, err := strconv.ParseBool(raw)
	if err != nil {
		return true
	}
	return off
}

// Enabled reports whether the agent can be used: the flag is on and the skill
// loads with the expected name.
func Enabled() bool {
	if !enabledFromEnv() {
		return false
	}
	_, err := LoadWritingCopilotSystemPrompt()
	return err == nil
}

// getenvModel returns LLM_MODEL_WRITING_COPILOT (empty = project default).
func getenvModel() string {
	return strings.TrimSpace(os.Getenv(envModel))
}

// getenvMaxTokens returns WRITING_COPILOT_MAX_TOKENS (0 = auto per model).
func getenvMaxTokens() int {
	raw := strings.TrimSpace(os.Getenv(envMaxTokens))
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1024 {
		log.Printf("[writingcopilot] %s=%q invalido: se calcula por modelo", envMaxTokens, raw)
		return 0
	}
	return v
}

// maxTokensFor returns the completion budget for a request. A pack of N shots
// needs roughly N*maxTokensPerShot tokens; the budget grows with the shot
// count, capped by the model's output limit and by the env override.
func maxTokensFor(model string, shots int) int {
	limit := outputLimitFor(model)
	if env := getenvMaxTokens(); env > 0 && env < limit {
		limit = env
	}
	need := maxTokensBase + shots*maxTokensPerShot
	if need > limit {
		need = limit
	}
	if need < maxTokensBase {
		need = maxTokensBase
	}
	return need
}

// outputLimitFor is the max output tokens published by the provider for the
// text models the copilot can use.
func outputLimitFor(model string) int {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini"):
		return 65000
	case strings.Contains(m, "claude"):
		return 64000
	case strings.Contains(m, "gpt-4o-mini"), strings.Contains(m, "gpt-4o"):
		return 16000
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return 100000
	default:
		return 16000
	}
}

// copilotTemperature reads WRITING_COPILOT_TEMPERATURE (default 0.7, 0..2).
func copilotTemperature() float64 {
	raw := strings.TrimSpace(os.Getenv(envTemperature))
	if raw == "" {
		return defaultTemperature
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 2 {
		log.Printf("[writingcopilot] %s=%q invalido: se usa %.1f", envTemperature, raw, defaultTemperature)
		return defaultTemperature
	}
	return v
}

// ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ HTTP ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬ÃƒÆ’Ã‚Â¢ÃƒÂ¢Ã¢â€šÂ¬Ã‚ÂÃƒÂ¢Ã¢â‚¬Å¡Ã‚Â¬

// historyMessage is one previous turn of the conversation.
type historyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatParams mirrors the skill parameter names (Section 3 of the document)
// plus shot_count, which the user states in plain language.
type chatParams struct {
	OutputLanguage string   `json:"output_language"` // es | en | bilingual
	Delivery       string   `json:"delivery"`        // sequence | shot_pack | auto
	AspectRatio    string   `json:"aspect_ratio"`
	TotalDurationS *float64 `json:"total_duration_s"`
	MaxClipS       *float64 `json:"max_clip_s"`
	FPS            *float64 `json:"fps"`
	Look           string   `json:"look"`
	LookID         string   `json:"look_id"`
	LookIntensity  string   `json:"look_intensity"`
	LookSource     string   `json:"look_source"`
	LookRef        string   `json:"look_ref"`
	AudioMode      string   `json:"audio_mode"`    // diegetic_only | with_dialogue | with_music_notes | silent
	TargetModel    string   `json:"target_model"`  // video model the piece is written for
	ResponseMode   string   `json:"response_mode"` // chat | prompt_only
	ShotCount      *int     `json:"shot_count"`    // cuts inside the piece
	CutsPerShot    *int     `json:"cuts_per_shot"` // framing changes inside each shot
	// VideoType selects the step-1 structure guide: commercial_short |
	// commercial_long | story_short | story_long.
	VideoType string `json:"video_type"`
	// Lighting is the art-direction/lighting block chosen in the UI.
	Lighting string `json:"lighting"`
	// CameraMovements is the ordered camera-movement block chosen in the UI.
	CameraMovements string `json:"camera_movements"`
}

// chatRequest is POST /writing-copilot/chat.
type chatRequest struct {
	Message string `json:"message"`
	// Model is the catalog name of the brain (openrouter/... or anthropic/...).
	Model      string           `json:"model"`
	History    []historyMessage `json:"history"`
	Params     *chatParams      `json:"params"`
	References string           `json:"references"`
	// ReferenceFiles are uploaded image ids the brain receives as vision input.
	ReferenceFiles []string `json:"reference_files"`
}

// chatResponse is the agent answer plus its copyable prompt blocks.
type chatResponse struct {
	Text    string   `json:"text"`
	Prompts []string `json:"prompts"`
	Model   string   `json:"model"`
	Usage   *usage   `json:"usage,omitempty"`
}

const (
	maxReferenceImages     = 6
	maxReferenceImageBytes = 5 << 20
)

// loadReferenceImages reads the uploaded images the brain should see.
func (m *Module) loadReferenceImages(c *gin.Context, ids []string, provider string) []map[string]any {
	if len(ids) == 0 || m.files == nil {
		return nil
	}
	hdl, ok := m.files(c)
	if !ok || hdl == nil {
		return nil
	}
	images := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		if len(images) >= maxReferenceImages {
			break
		}
		info, err := hdl.Info(id)
		if err != nil || info == nil || !strings.HasPrefix(info.MimeType, "image/") {
			continue
		}
		path, err := hdl.LocalPath(id)
		if err != nil {
			continue
		}
		st, err := os.Stat(path)
		if err != nil || st.Size() > maxReferenceImageBytes {
			log.Printf("[writingcopilot] imagen %s omitida (pesa demasiado)", info.Filename)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		images = append(images, imagePart(provider, info.MimeType, data))
	}
	return images
}

// Availability handles GET /writing-copilot/availability.
func (m *Module) Availability(c *gin.Context) {
	utils.Success(c, gin.H{"enabled": Enabled()})
}

// Chat handles POST /writing-copilot/chat.
func (m *Module) Chat(c *gin.Context) {
	if !Enabled() {
		utils.Error(c, http.StatusServiceUnavailable, "el agente Writing Copilot esta deshabilitado")
		return
	}
	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	req.Message = strings.TrimSpace(req.Message)
	if req.Message == "" {
		utils.BadRequest(c, "falta el mensaje (idea, guion o historia)")
		return
	}
	if len(req.Message) > maxMessageChars {
		utils.BadRequest(c, fmt.Sprintf("el mensaje supera los %d caracteres", maxMessageChars))
		return
	}
	videoType := ""
	shots := 0
	if req.Params != nil {
		videoType = strings.TrimSpace(req.Params.VideoType)
		if req.Params.ShotCount != nil {
			shots = *req.Params.ShotCount
			if shots < 0 {
				shots = 0
			}
			if shots > maxShotsPerPiece {
				shots = maxShotsPerPiece
				req.Params.ShotCount = &shots
			}
		}
	}
	if !ValidVideoType(videoType) {
		utils.BadRequest(c, "video_type invalido: usa commercial_short, commercial_long, story_short o story_long")
		return
	}
	hdl, ok := m.creds(c)
	if !ok {
		return
	}
	provider, cred, model, rerr := resolveBrain(hdl, req.Model)
	if rerr != nil {
		utils.BadRequest(c, rerr.Error())
		return
	}
	system, err := SystemPromptForVideoType(videoType)
	if err != nil {
		utils.Error(c, http.StatusServiceUnavailable, "el skill de Writing Copilot no esta disponible")
		return
	}
	started := time.Now()
	log.Printf("[writingcopilot] chat provider=%s model=%s video_type=%q shots=%d system_bytes=%d history=%d message_chars=%d",
		provider, model, videoType, shots, len(system), len(req.History), len(req.Message))

	ctx, cancel := context.WithTimeout(c.Request.Context(), callTimeout)
	defer cancel()
	images := m.loadReferenceImages(c, req.ReferenceFiles, provider)
	if len(images) > 0 {
		log.Printf("[writingcopilot] %d imagen(es) de referencia enviadas al cerebro", len(images))
	}
	budget := maxTokensFor(model, shots)
	log.Printf("[writingcopilot] completion budget=%d tokens (model=%s shots=%d)", budget, model, shots)
	text, u, err := m.brain.chat(ctx, provider, cred, model, system, buildUserMessage(req, images), copilotTemperature(), budget)
	if err != nil {
		utils.Error(c, http.StatusBadGateway, err.Error())
		return
	}
	rawChars := len(text)
	text = normalizePromptFences(text)
	if len(text) != rawChars {
		log.Printf("[writingcopilot] respuesta sin vallas: se normalizaron los bloques de tomas")
	}
	prompts := extractPrompts(text)
	log.Printf("[writingcopilot] chat listo en %s: %d chars, %d bloques, tokens_in=%d tokens_out=%d",
		time.Since(started).Round(time.Millisecond), len(text), len(prompts), u.PromptTokens, u.CompletionTokens)
	utils.Success(c, chatResponse{Text: text, Prompts: prompts, Model: model, Usage: &u})
}

// buildUserMessage assembles PARAMS (only the fields sent), REFERENCES and the
// user message, after the previous history turns.
func buildUserMessage(req chatRequest, images []map[string]any) []map[string]any {
	messages := make([]map[string]any, 0, len(req.History)+1)
	history := req.History
	if len(history) > maxHistoryMessages {
		history = history[len(history)-maxHistoryMessages:]
	}
	for _, item := range history {
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(item.Content)
		if content == "" {
			continue
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	sections := make([]string, 0, 3)
	if params := renderParams(req.Params); params != "" {
		sections = append(sections, "PARAMS:\n"+params)
	}
	if refs := strings.TrimSpace(req.References); refs != "" {
		sections = append(sections, "REFERENCES:\n"+refs)
	}
	sections = append(sections, strings.TrimSpace(req.Message))
	text := strings.Join(sections, "\n\n")
	if len(images) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": text})
		return messages
	}
	// With images the turn travels as blocks: text first, then the photos, so
	// the brain sees the reference assets and writes a coherent prompt.
	content := make([]map[string]any, 0, len(images)+1)
	content = append(content, map[string]any{"type": "text", "text": text})
	content = append(content, images...)
	messages = append(messages, map[string]any{"role": "user", "content": content})
	return messages
}

// renderParams writes one `key: value` line per informed parameter.
func renderParams(p *chatParams) string {
	if p == nil {
		return ""
	}
	lines := make([]string, 0, 9)
	add := func(key, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, key+": "+v)
		}
	}
	add("output_language", p.OutputLanguage)
	add("delivery", p.Delivery)
	add("aspect_ratio", p.AspectRatio)
	if p.TotalDurationS != nil {
		add("total_duration_s", strconv.FormatFloat(*p.TotalDurationS, 'f', -1, 64))
	}
	if p.MaxClipS != nil {
		add("max_clip_s", strconv.FormatFloat(*p.MaxClipS, 'f', -1, 64))
	}
	if p.FPS != nil {
		add("fps", strconv.FormatFloat(*p.FPS, 'f', -1, 64))
	}
	lookAssembled := false
	if lookID := strings.TrimSpace(p.LookID); lookID != "" {
		if block, err := looks.Assemble(looks.ModeVideo, lookID, looks.NormalizeLevel(p.LookIntensity), p.LookSource, p.LookRef); err == nil {
			add("look", block)
			lookAssembled = true
		}
	}
	if look := strings.TrimSpace(p.Look); look != "" {
		if lookAssembled {
			add("look_description", look)
		} else {
			add("look", look)
		}
	}
	add("audio_mode", p.AudioMode)
	add("target_model", p.TargetModel)
	add("response_mode", p.ResponseMode)
	if p.ShotCount != nil && *p.ShotCount > 0 {
		add("shot_count", strconv.Itoa(*p.ShotCount))
	}
	if p.CutsPerShot != nil && *p.CutsPerShot > 0 {
		add("cuts_per_shot", strconv.Itoa(*p.CutsPerShot))
	}
	// Tomas = clips: cada toma dura total/shots y los cortes se reparten dentro
	// de ese clip, para que el cerebro no confunda duración total con clip.
	if p.ShotCount != nil && *p.ShotCount > 0 && p.TotalDurationS != nil && *p.TotalDurationS > 0 {
		perShot := *p.TotalDurationS / float64(*p.ShotCount)
		add("shot_duration_s", formatSeconds(perShot))
		if p.CutsPerShot != nil && *p.CutsPerShot > 0 {
			add("cut_interval_s", formatSeconds(perShot/float64(*p.CutsPerShot)))
		}
	}
	add("video_type", p.VideoType)
	if !lookAssembled {
		add("lighting", p.Lighting)
	}
	add("camera_movements", p.CameraMovements)
	return strings.Join(lines, "\n")
}

var promptBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\n?(.*?)```")

// formatSeconds trims a duration to at most two decimals ("5", "2.5", "2.33").
func formatSeconds(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// shotMarker finds the start of each shot in a shot-pack answer (the skill
// asks for `PROMPT n de N` headers, older runs may use `PLANO n`).
var shotMarker = regexp.MustCompile(`(?mi)^(PROMPT\s+\d+\s+de\s+\d+|PLANO\s+\d+)\b.*$`)

// normalizePromptFences wraps every shot of an unwrapped shot pack in its own
// code fence. The skill asks the model for fenced blocks, but long answers
// (15 shots) sometimes come back as plain text; the front splits the answer by
// fences, so without this the shots are not detected.
func normalizePromptFences(text string) string {
	if strings.Contains(text, "```") {
		return text
	}
	starts := shotMarker.FindAllStringIndex(text, -1)
	if len(starts) < 2 {
		return text
	}
	var sb strings.Builder
	if head := strings.TrimSpace(text[:starts[0][0]]); head != "" {
		sb.WriteString(head)
		sb.WriteString("\n\n")
	}
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		block := strings.TrimSpace(text[start[0]:end])
		if block == "" {
			continue
		}
		sb.WriteString("```\n")
		sb.WriteString(block)
		sb.WriteString("\n```\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// extractPrompts returns the body of every fenced code block, in order.
func extractPrompts(text string) []string {
	matches := promptBlock.FindAllStringSubmatch(text, -1)
	prompts := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		body := strings.TrimSpace(match[1])
		if body != "" {
			prompts = append(prompts, body)
		}
	}
	return prompts
}
