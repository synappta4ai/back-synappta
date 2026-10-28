// artifacts.go: turnos de artefacto estructurado del flujo /agency.
// Generaliza el sales_angles puntual a los tres pasos de los tres agentes:
// mapeo agente+paso → kind, extracción tolerante de JSON y validadores por
// contrato (los .md definen la SALIDA de cada paso — ver
// docs/plan-flujos-agentes.md §2.3, §3–5 y §6).
package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Kinds de artefacto que acepta chatRequest.Artifact; cada uno fija el
// contrato de SALIDA que el back valida.
const (
	artifactSalesAngles   = "sales_angles"   // paso 1 inmobiliaria
	artifactScenePrompts  = "scene_prompts"  // paso 1 comercial
	artifactSceneAnalysis = "scene_analysis" // paso 1 cine
	artifactStoryboard    = "storyboard"     // paso 2 (los 3 agentes)
	artifactClipPrompt    = "clip_prompt"    // paso 3 (los 3 agentes)
)

// durTolerance: tolerancia (en segundos) al comparar sumas de duración
// contra el total pedido (redondeos del modelo).
const durTolerance = 0.01

// equalDurTolerance: tolerancia al exigir viñetas de igual duración (10 s / 3
// = 3,33 s redondeado a dos decimales).
const equalDurTolerance = 0.05

// timeRangeRe detecta rangos de tiempo estilo "0-3 s" / "3.5-5 segundos"
// dentro de un prompt_video para verificar que cubran el total.
var timeRangeRe = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*-\s*(\d+(?:[.,]\d+)?)\s*s`)

// financialRe detecta promesas financieras prohibidas en ángulos
// (regla: sin prometer rentabilidad ni plusvalía).
var financialRe = regexp.MustCompile(`(?i)rentabilid|plusval|garantizad|ganancia asegurada|retorno garantizado`)

// sceneTagRe detecta tags de referencia (`@tienda`) dentro de los campos
// visuales de un prompt de escena comercial.
var sceneTagRe = regexp.MustCompile(`(?i)@[a-z0-9][a-z0-9-]*`)

// artifactSpec es el artefacto (kind + paso) que el front pide en un turno.
type artifactSpec struct {
	Kind string
	Step int
}

// parseArtifactKind valida el campo artifact del request y devuelve su
// paso asociado. false si el kind no existe.
func parseArtifactKind(kind string) (artifactSpec, bool) {
	switch kind {
	case artifactSalesAngles, artifactScenePrompts, artifactSceneAnalysis:
		return artifactSpec{Kind: kind, Step: 1}, true
	case artifactStoryboard:
		return artifactSpec{Kind: artifactStoryboard, Step: 2}, true
	case artifactClipPrompt:
		return artifactSpec{Kind: artifactClipPrompt, Step: 3}, true
	}
	return artifactSpec{}, false
}

// stepArtifact devuelve el artefacto estándar del paso para un agente
// (paso 1: sales_angles en inmobiliaria, scene_analysis en cine y
// scene_prompts en comercial).
func stepArtifact(workflow string, step int) artifactSpec {
	switch step {
	case 1:
		switch workflow {
		case workflowRealEstate:
			return artifactSpec{Kind: artifactSalesAngles, Step: 1}
		case workflowCinema:
			return artifactSpec{Kind: artifactSceneAnalysis, Step: 1}
		default:
			return artifactSpec{Kind: artifactScenePrompts, Step: 1}
		}
	case 2:
		return artifactSpec{Kind: artifactStoryboard, Step: 2}
	case 3:
		return artifactSpec{Kind: artifactClipPrompt, Step: 3}
	}
	return artifactSpec{}
}

// extractArtifactJSON saca el objeto JSON de la respuesta del LLM
// tolerando fences markdown y texto alrededor (mismo criterio que
// extractAngles, generalizado a cualquier contrato).
func extractArtifactJSON(raw string) (map[string]any, bool) {
	s := strings.TrimSpace(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s[start:end+1]), &parsed); err != nil || len(parsed) == 0 {
		return nil, false
	}
	return parsed, true
}

// decodePayload parsea el campo payload del request (contexto del paso:
// brief, cantidades, referencias) a JSON arbitrario. Vacío si no vino.
func decodePayload(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// validateArtifact revisa el artefacto contra el contrato del .md y
// devuelve advertencias (vacío = contrato cumplido). Cuando el estado es
// necesita_informacion no valida nada: el turno se resuelve con preguntas.
func validateArtifact(spec artifactSpec, data, payload map[string]any) []string {
	if strField(data, "estado") == "necesita_informacion" {
		return nil
	}
	switch spec.Kind {
	case artifactSalesAngles:
		return validateSalesAngles(data, payload)
	case artifactScenePrompts, artifactSceneAnalysis:
		return validateScenePrompts(data, payload, spec.Kind == artifactSceneAnalysis)
	case artifactStoryboard:
		return validateStoryboard(data, payload)
	case artifactClipPrompt:
		return validateClipPrompt(data, payload)
	}
	return nil
}

// validateSalesAngles: cantidad exacta de ángulos, título y promesa en
// cada uno, y sin promesas financieras (solo advertencia).
func validateSalesAngles(data, payload map[string]any) []string {
	warns := []string{}
	angles := objArr(data["angulos"])
	if len(angles) == 0 {
		return []string{"falta el arreglo \"angulos\" con al menos un ángulo"}
	}
	if want, ok := intField(payload, "cantidad_angulos"); ok && len(angles) != want {
		warns = append(warns, fmt.Sprintf("pediste %d ángulos y el JSON trae %d", want, len(angles)))
	}
	for i, a := range angles {
		if strField(a, "titulo") == "" {
			warns = append(warns, fmt.Sprintf("ángulo %d sin \"titulo\"", i+1))
		}
		promesa := strField(a, "promesa")
		if promesa == "" {
			warns = append(warns, fmt.Sprintf("ángulo %d sin \"promesa\"", i+1))
			continue
		}
		if financialRe.MatchString(promesa) {
			warns = append(warns, fmt.Sprintf("ángulo %d con promesa financiera prohibida (rentabilidad/plusvalía/garantizado)", i+1))
		}
	}
	return warns
}

// validateScenePrompts: cantidad exacta de prompts y beats no vacíos en
// cada uno; en cine exige además la biblia de continuidad. En comercial
// revisa que los tags `@archivo` de la escena existan en las referencias
// que el usuario ya subió.
func validateScenePrompts(data, payload map[string]any, cinema bool) []string {
	warns := []string{}
	prompts := objArr(data["prompts"])
	if len(prompts) == 0 {
		return []string{"falta el arreglo \"prompts\" con al menos una escena"}
	}
	if want, ok := intField(payload, "cantidad_prompts"); ok && len(prompts) != want {
		warns = append(warns, fmt.Sprintf("pediste %d prompts y el JSON trae %d", want, len(prompts)))
	}
	for i, p := range prompts {
		if len(objArr(p["beats"])) == 0 {
			warns = append(warns, fmt.Sprintf("prompt %d sin \"beats\"", i+1))
		}
	}
	if cinema {
		biblia, _ := data["biblia_continuidad"].(map[string]any)
		if len(objArr(biblia["personajes"])) == 0 {
			warns = append(warns, "falta \"biblia_continuidad.personajes\" (referencia canónica de cada personaje)")
		}
	} else {
		warns = append(warns, unknownSceneTags(prompts, payload)...)
	}
	return warns
}

// unknownSceneTags avisa cuando la escena referencia con `@tag` un archivo
// que no está en `referencias_subidas` (el front lo inserta desde la foto y
// un tag inventado no adjunta nada al video). Sin referencias no opina.
func unknownSceneTags(prompts []map[string]any, payload map[string]any) []string {
	known := map[string]bool{}
	for _, ref := range objArr(payload["referencias_subidas"]) {
		if tag := strings.ToLower(strings.TrimSpace(strField(ref, "tag"))); tag != "" {
			known[tag] = true
		}
	}
	if len(known) == 0 {
		return nil
	}
	warns := []string{}
	for i, p := range prompts {
		text := strings.Join([]string{
			strField(p, "locacion"),
			strField(p, "producto_en_escena"),
			strings.Join(strArr(p["personajes"]), " "),
		}, " ")
		seen := map[string]bool{}
		for _, tag := range sceneTagRe.FindAllString(text, -1) {
			key := strings.ToLower(tag)
			if known[key] || seen[key] {
				continue
			}
			seen[key] = true
			warns = append(warns, fmt.Sprintf("prompt %d usa %s, que no está en \"referencias_subidas\"", i+1, tag))
		}
	}
	return warns
}

// validateStoryboard: cantidad exacta de viñetas, duración que suma el
// total pedido, ninguna viñeta bajo el mínimo y la hoja escrita.
func validateStoryboard(data, payload map[string]any) []string {
	warns := []string{}
	vinetas := objArr(data["vinetas"])
	if len(vinetas) == 0 {
		return []string{"falta el arreglo \"vinetas\""}
	}
	if want, ok := intField(payload, "cantidad_vinetas"); ok && len(vinetas) != want {
		warns = append(warns, fmt.Sprintf("pediste %d viñetas y el JSON trae %d", want, len(vinetas)))
	}
	total, hasTotal := numField(data, "duracion_total_s")
	if !hasTotal {
		total, hasTotal = numField(payload, "duracion_total_s")
	}
	minS, hasMin := numField(payload, "duracion_minima_vineta_s")
	var sum float64
	for i, v := range vinetas {
		d, ok := numField(v, "duracion_s")
		if !ok {
			warns = append(warns, fmt.Sprintf("viñeta %d sin \"duracion_s\"", i+1))
			continue
		}
		sum += d
		if hasMin && d < minS-durTolerance {
			warns = append(warns, fmt.Sprintf("viñeta %d dura %.2f s, menos del mínimo %.2f s", i+1, d, minS))
		}
	}
	if hasTotal && abs(sum-total) > durTolerance {
		warns = append(warns, fmt.Sprintf("las duraciones suman %.2f s y el total pedido es %.2f s", sum, total))
	}
	// La duración se reparte en partes iguales: total / cantidad de viñetas
	// (4 viñetas en 10 s = 2,5 s cada una).
	if hasTotal {
		each := total / float64(len(vinetas))
		for i, v := range vinetas {
			if d, ok := numField(v, "duracion_s"); ok && abs(d-each) > equalDurTolerance {
				warns = append(warns, fmt.Sprintf("las viñetas deben durar lo mismo: %.2f s cada una (%.2f s / %d viñetas) y la %d dura %.2f s", each, total, len(vinetas), i+1, d))
				break
			}
		}
	}
	if strField(data, "prompt_hoja_storyboard") == "" {
		warns = append(warns, "falta \"prompt_hoja_storyboard\"")
	}
	return warns
}

// validateClipPrompt: prompt_video presente, cada referencia declarada al
// menos una vez y los tiempos cubriendo el total del clip.
func validateClipPrompt(data, payload map[string]any) []string {
	warns := []string{}
	video := strField(data, "prompt_video")
	if video == "" {
		return []string{"falta \"prompt_video\""}
	}
	for _, ref := range objArr(payload["referencias"]) {
		tag := strField(ref, "tag")
		if tag != "" && !strings.Contains(video, tag) {
			warns = append(warns, fmt.Sprintf("la referencia %s no aparece declarada en prompt_video", tag))
		}
	}
	total, hasTotal := numField(data, "duracion_total_s")
	if !hasTotal {
		total, hasTotal = numField(payload, "duracion_total_s")
	}
	if !hasTotal {
		return warns
	}
	maxEnd, found := maxTimeEnd(video)
	if found && abs(maxEnd-total) > 0.25 {
		warns = append(warns, fmt.Sprintf("los tiempos de prompt_video llegan a %.2f s y el total del clip es %.2f s", maxEnd, total))
	}
	return warns
}

// artifactQuestions extrae las preguntas de un artefacto que pide
// información; nil si no lo pide.
func artifactQuestions(data map[string]any) []string {
	if strField(data, "estado") != "necesita_informacion" {
		return nil
	}
	out := []string{}
	for _, q := range strArr(data["preguntas_pendientes"]) {
		if s := strings.TrimSpace(q); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// normalizeSalesAngles agrega al artefacto extendido del paso 1
// inmobiliario el arreglo legacy "angles" (title/hook/narrative_pitch/
// target_audience) que el front actual todavía consume, derivado de los
// campos nuevos — ver docs/plan-flujos-agentes.md §4.
func normalizeSalesAngles(data map[string]any) {
	legacy := make([]map[string]any, 0)
	for i, a := range objArr(data["angulos"]) {
		title := strField(a, "titulo")
		if title == "" {
			title = fmt.Sprintf("Ángulo %d", i+1)
		}
		legacy = append(legacy, map[string]any{
			"title":           title,
			"hook":            strField(a, "gancho_cierre"),
			"narrative_pitch": strField(a, "promesa"),
			"target_audience": strField(a, "comprador_ideal"),
			"selected":        false,
		})
	}
	if len(legacy) > 0 {
		data["angles"] = legacy
	}
}

// appendWarnings suma advertencias al arreglo del artefacto para que el
// front las muestre sin fallar el turno (regla: un solo reintento y
// después se devuelve con advertencias — §6.2 del plan).
func appendWarnings(data map[string]any, warns []string) {
	existing := strArr(data["advertencias"])
	data["advertencias"] = append(existing, warns...)
}

// correctionMessage arma el mensaje de un solo reintento: le pide al LLM
// corregir el JSON según las advertencias del validador.
func correctionMessage(warns []string) string {
	return "Tu respuesta no cumple el contrato del paso:\n- " +
		strings.Join(warns, "\n- ") +
		"\nRehacé el JSON completo corregido, sin markdown y sin texto fuera del JSON."
}

// strArr convierte un JSON arbitrario a arreglo de strings tolerando
// tipos inesperados (los no-string se descartan).
func strArr(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// objArr convierte un JSON arbitrario a arreglo de objetos tolerando
// tipos inesperados (devuelve vacío si no es un arreglo de objetos).
func objArr(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// intField lee un campo numérico entero de JSON arbitrario.
func intField(m map[string]any, key string) (int, bool) {
	f, ok := numField(m, key)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// numField lee un campo numérico de JSON arbitrario (acepta enteros y
// flotantes).
func numField(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

// maxTimeEnd devuelve el mayor tiempo final ("N s") encontrado en el
// texto; false si no hubo rangos reconocibles.
func maxTimeEnd(text string) (float64, bool) {
	var max float64
	found := false
	for _, m := range timeRangeRe.FindAllStringSubmatch(text, -1) {
		if len(m) != 3 {
			continue
		}
		end, err := parseFloat(m[2])
		if err != nil {
			continue
		}
		if !found || end > max {
			max, found = end, true
		}
	}
	return max, found
}

// parseFloat acepta coma o punto como separador decimal.
func parseFloat(s string) (float64, error) {
	var out float64
	_, err := fmt.Sscan(strings.ReplaceAll(s, ",", "."), &out)
	return out, err
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
