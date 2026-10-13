// prompts.go: arma el system prompt de cada paso del flujo /agency a partir
// de los .md madre (docs/agente-*.md, embebidos vía el paquete synapta/docs).
// Los .md ya están escritos con encabezados fijos (ROL, REGLAS COMUNES,
// MÓDULO DE ACTUACIÓN, PASO N, LISTA DE CONTROL) precisamente para esto —
// ver docs/plan-flujos-agentes.md §2.2.
package agent

import (
	"fmt"
	"strings"
	"sync"

	"synapta/docs"
)

// stepCount: pasos con artefacto del flujo /agency (1..3; el paso 0 del
// brief es determinista en el front y no lleva system prompt).
const stepCount = 3

// agentDoc son las secciones parseadas de un .md madre.
type agentDoc struct {
	rol       string
	reglas    string
	modulo    string // vacío si el agente no lo tiene (inmobiliaria)
	steps     [stepCount + 1]string
	checklist string
}

// parseAgentDoc separa un .md madre por encabezados "## ". Los encabezados
// "### " quedan dentro de su sección madre (no abren sección nueva).
func parseAgentDoc(md string) agentDoc {
	var doc agentDoc
	var current *string
	for _, line := range strings.Split(md, "\n") {
		if !strings.HasPrefix(line, "## ") {
			if current != nil {
				*current += line + "\n"
			}
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(line, "## "))
		switch {
		case strings.HasPrefix(heading, "ROL"):
			current = &doc.rol
		case strings.HasPrefix(heading, "REGLAS COMUNES"):
			current = &doc.reglas
		case strings.HasPrefix(heading, "MÓDULO DE ACTUACIÓN"):
			current = &doc.modulo
		case strings.HasPrefix(heading, "LISTA DE CONTROL"):
			current = &doc.checklist
		case strings.HasPrefix(heading, "PASO "):
			n := 0
			if _, err := fmt.Sscanf(heading, "PASO %d", &n); err == nil && n >= 1 && n <= stepCount {
				current = &doc.steps[n]
			} else {
				current = nil
			}
		default:
			current = nil
		}
		if current != nil {
			*current = line + "\n" // el encabezado viaja con la sección
		}
	}
	return doc
}

// agentDocs carga (una vez) y cachea los .md parseados por workflow. Si un
// .md falta, ese workflow queda sin secciones y systemFor devuelve vacío
// (el caller cae al prompt de la persona — ver direct.go).
var agentDocs = sync.OnceValue(func() map[string]agentDoc {
	files := map[string]string{
		workflowCommercial: "agente-comercial.md",
		workflowRealEstate: "agente-inmobiliaria.md",
		workflowCinema:     "agente-cine.md",
	}
	out := make(map[string]agentDoc, len(files))
	for workflow, name := range files {
		raw, err := docs.AgentMD.ReadFile(name)
		if err != nil {
			continue
		}
		out[workflow] = parseAgentDoc(string(raw))
	}
	return out
})

// systemFor arma el system prompt del paso N del agente: ROL + REGLAS
// COMUNES + MÓDULO DE ACTUACIÓN (si el agente lo tiene) + PASO N + LISTA
// DE CONTROL. Devuelve vacío si el agente no tiene .md o el paso no existe.
func systemFor(workflow string, step int) string {
	if step < 1 || step > stepCount {
		return ""
	}
	doc := agentDocs()[workflowSlug(workflow)]
	parts := []string{doc.rol, doc.reglas, doc.modulo, doc.steps[step], doc.checklist}
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}
