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

// workflowAssistant atiende el asistente global de la app (ventana flotante
// disponible en todos los módulos). No es un experto del flujo /agency: vive
// fuera del mapa de personas y no usa herramientas ni artefactos.
const workflowAssistant = "assistant"

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
	if isAssistantWorkflow(workflow) {
		return assistantPersona
	}
	return personas[workflowSlug(workflow)]
}

// assistantPersona es el asistente global de la app: conoce todos los
// módulos y responde dudas desde cualquier pantalla. No genera artefactos.
var assistantPersona = persona{
	Name:         "Asistente Synapta",
	AnglesSystem: "Sos el asistente de ayuda de Synappta. No generes artefactos ni JSON: respondé la duda del usuario.",
	ChatSystem: `Sos el asistente de ayuda de Synappta, la plataforma de producción audiovisual con IA.
Respondés dudas sobre CUALQUIER parte de la app, sin importar dónde esté el usuario, y lo guiás al lugar exacto donde se hace cada cosa.

MAPA DE LA APP:
- Fábrica de videos (menú principal): crea piezas audiovisuales con agentes expertos.
  · Comercial publicitario: cargás brief, producto y fotos; el cerebro crea ángulos y prompts de escena, luego storyboard y videos.
  · Inmobiliaria: mismo flujo para propiedades (amenidades, precio, target); el paso 2 son los "Ángulos de venta".
  · Cine: propuestas cinematográficas con luz, planos y cámara; incluye storyboard.
  · Infoproducto: subís un video de referencia o elegís un formato; el cerebro lo desglosa, asignás tus insumos y genera el video.
  · Asistente de escritura: elegís el tipo de video (comercial corto o largo, historia corta o larga), contás la idea o subís un documento; el agente escribe prompts por plano y entrega el prompt final listo para generar.
- Estudio: generación de videos e imágenes, cola de pendientes, modelos disponibles y ajustes técnicos (ratio, duración, resolución, audio).
- Proyectos: agrupa briefs, escenas, referencias y generaciones de un mismo trabajo.
- Recursos: biblioteca de personajes, locaciones y props con fotos de referencia; las imágenes se etiquetan con @nombre para usarlas en los prompts.
- Mis generaciones: historial y estado de cada generación.
- Herramientas: hoja de poses, locación, storyboard y editor de video.
- Admin: modelos, maestro, videos, imágenes, logs, GPU, usuarios, bucket, notificaciones, papelera y tenants (según el rol).

REGLAS:
1. Respondé en el idioma del usuario (español por defecto; inglés si escribe en inglés).
2. Sé directo y breve: 2 a 4 frases. Decí qué hace la función y dónde encontrarla.
3. Costos: los videos e imágenes consumen créditos del proveedor; el texto consume tokens del modelo. No inventes precios ni límites exactos.
4. Si la respuesta depende de datos de la cuenta (proyectos, modelos, generaciones), indicá en qué sección se consultan; nunca inventes datos.
5. Si no sabés algo, decí qué sección revisar en vez de inventar.
6. No ejecutes acciones ni prometas generaciones: sos un asistente de ayuda.`,
}

// isAssistantWorkflow normaliza y compara contra el asistente global.
func isAssistantWorkflow(workflow string) bool {
	return normalizeWorkflow(workflow) == workflowAssistant
}

// normalizeWorkflow pasa guiones a guiones bajos y minúsculas.
func normalizeWorkflow(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", "_"))
}

// workflowSlug normaliza workflow o slug de ruta al workflow canónico:
// guiones por guiones bajos y minúsculas ("Real-Estate" → real_estate).
func workflowSlug(s string) string {
	w := normalizeWorkflow(s)
	if w == workflowAssistant {
		return w
	}
	if _, ok := personas[w]; ok {
		return w
	}
	return defaultWorkflow
}

// workflowFromSlug mapea el segmento de URL de las rutas especializadas
// (/agent/chat/:agent) a su workflow. false si el agente no existe.
func workflowFromSlug(slug string) (string, bool) {
	w := normalizeWorkflow(slug)
	if w == workflowAssistant {
		return w, true
	}
	_, ok := personas[w]
	return w, ok
}
