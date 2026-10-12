// personas.go: los 3 agentes expertos del flujo /agency, uno por
// workflow (selector "Agente" del paso 1). Cada persona define los
// system prompts con los que el back responde el turno sin agent-server:
// uno para el artefacto sales_angles del paso 2 y otro para el chat
// asistente que acompaña al usuario a crear su video.
package agent

import "strings"

// anglesContract es el artefacto sales_angles que el front convierte en
// ángulos (anglesFromAgent): mismo esquema para los tres expertos.
const anglesContract = `Respondé EXCLUSIVAMENTE con JSON válido, sin markdown y sin texto fuera del JSON:
{"angles":[{"title":"...","hook":"...","narrative_pitch":"...","target_audience":"...","selected":false}]}
Respetá exactamente la cantidad de ángulos que el mensaje pide.`

// defaultWorkflow es el experto ante un workflow desconocido o vacío
// (coincide con el default del selector del front).
const defaultWorkflow = "commercial"

// Workflow válidos del flujo /agency.
const (
	workflowCommercial = "commercial"
	workflowRealEstate = "real_estate"
	workflowCinema     = "cinema"
)

// persona de un agente experto: su nombre y sus dos system prompts.
type persona struct {
	// Name es el nombre humano del experto (logs y tests).
	Name string
	// AnglesSystem se usa cuando el turno pide el artefacto
	// sales_angles: exige el JSON del contrato sin prosa alrededor.
	AnglesSystem string
	// ChatSystem es el asistente conversacional que ayuda al usuario
	// a crear su video dentro de su especialidad.
	ChatSystem string
}

// personas indexa los expertos por workflow.
var personas = map[string]persona{
	workflowCommercial: {
		Name: "Comercial publicitario",
		AnglesSystem: `Sos el generador de ángulos de comunicación de Synapta para productos y servicios.
Pensás como estratega comercial: cada ángulo ataca un beneficio concreto, un target y un hook publicitario que hagan vendible el producto o servicio en un video.
` + anglesContract,
		ChatSystem: `Sos el estratega comercial de Synapta. Ayudás al usuario a crear un video publicitario de su producto o servicio: target del cliente, puntos fuertes, guion del comercial y ángulos de comunicación. Respondé en español, tono profesional y conciso.`,
	},
	workflowRealEstate: {
		Name: "Inmobiliaria",
		AnglesSystem: `Sos el generador de ángulos de venta de Synapta para emprendimientos inmobiliarios.
` + anglesContract,
		ChatSystem: `Sos el asistente inmobiliario de Synapta. Ayudás al usuario a crear un video de su emprendimiento o propiedad: amenidades, precio promedio, target comprador y ángulos de venta. Respondé en español, tono profesional y conciso.`,
	},
	workflowCinema: {
		Name: "Cine",
		AnglesSystem: `Sos el generador de propuestas cinematográficas de Synapta.
Pensás como director de cine: cada ángulo es una escena con luz, planos, movimiento de cámara y tono que cuentan la idea del usuario.
` + anglesContract,
		ChatSystem: `Sos el director de cine de Synapta. Ayudás al usuario a crear su video: temática, guion, tono, referencias visuales, luz, planos y cámara. Respondé en español, tono profesional y conciso.`,
	},
}

// personaFor devuelve la persona del workflow; la desconocida o vacía
// cae en el default comercial. Acepta también el slug de ruta
// ("real-estate").
func personaFor(workflow string) persona {
	return personas[workflowSlug(workflow)]
}

// workflowSlug normaliza workflow o slug de ruta al workflow canónico:
// guiones por guiones bajos y minúsculas ("Real-Estate" → real_estate).
func workflowSlug(s string) string {
	w := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
	if _, ok := personas[w]; ok {
		return w
	}
	return defaultWorkflow
}

// workflowFromSlug mapea el segmento de URL de las rutas especializadas
// (/agent/chat/:agent) a su workflow. false si el agente no existe.
func workflowFromSlug(slug string) (string, bool) {
	w := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(slug), "-", "_"))
	_, ok := personas[w]
	return w, ok
}
