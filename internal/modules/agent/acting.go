package agent

import (
	"log"
	"sync"

	"synapta/internal/skills"
)

// actingEngineSection is section 6 of the infoproduct acting skill, loaded
// once. Prompt-producing artifacts append it to their system prompt so the
// characters act with micro-expressions and observable muscular facts.
var actingEngineSection = sync.OnceValue(func() string {
	acting, err := skills.ActingEngine()
	if err != nil {
		log.Printf("[agent] acting skill: %v", err)
		return ""
	}
	return acting
})

// withActingEngine appends the performance engine to a prompt-producing
// artifact system. Marketing angles do not get it: they are not performance
// prompts.
func withActingEngine(system, kind string) string {
	if kind == artifactSalesAngles {
		return system
	}
	acting := actingEngineSection()
	if acting == "" {
		return system
	}
	return system + "\n\n## ACTING ENGINE — APPLY TO EVERY CHARACTER PERFORMANCE\n\n" +
		"Use this acting engine in every character shot you write, including the final prompt: convert emotions into transitive actions and observable muscular facts, add one asynchronous micro-fidget in acting shots over 1 s, and translate prohibitions into positive locks. It enriches performance only: never change the story, camera, lighting, continuity or output format.\n\n" +
		acting
}
