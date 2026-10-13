// direct.go: responde el turno de chat del agente SIN el agent-server
// independiente. Cuando el proxy hacia localhost:3200 no responde, el back
// llama directo al LLM del tenant (OpenRouter o Anthropic) y emite los
// mismos eventos SSE que consume el front (meta + sales_angles | text),
// de modo que el flujo /agency no dependa de un servicio extra.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/modules/credential"
	"synapta/internal/tenancy"
	"synapta/internal/toolkit"
	"synapta/internal/utils"
)

const (
	// llmTimeout mata la llamada de chat antes de que el front abortue.
	llmTimeout = 40 * time.Second

	// artifactTimeout es más holgado: los artefactos ricos (prompt_video
	// de hasta 600 palabras) tardan más que un turno de chat.
	artifactTimeout = 60 * time.Second

	// chatMaxTokens limita el chat libre; artifactMaxTokens da margen a
	// los contratos largos de los .md (§2.7 del plan).
	chatMaxTokens     = 2048
	artifactMaxTokens = 4096

	openrouterBase  = "https://openrouter.ai/api/v1"
	openrouterPath  = "/chat/completions"
	openrouterModel = "openai/gpt-4o-mini"
	anthropicBase   = "https://api.anthropic.com"
	anthropicPath   = "/v1/messages"
	// claude-3-haiku-20240307 fue retirado del API (404 not_found_error).
	anthropicModel = "claude-haiku-4-5"
)

// toolsSystemHint se aplica al system del experto cuando el turno tiene
// herramientas: le dice que puede operar el tenant (y que debe consultar
// antes de inventar datos).
const toolsSystemHint = `

Tenés herramientas para operar la plataforma Synapta del usuario: listar y crear proyectos, listar modelos, generar videos e imágenes, consultar el estado de las generaciones y cancelar tareas. Usalas antes de responder cuando la respuesta dependa de datos reales (qué proyectos existen, qué modelos hay, cómo va una generación). Nunca inventes ids ni estados: si necesitás un dato, consultalo con la herramienta. Cuando generes un video o una imagen, informá el task_id para que el usuario pueda seguirlo en la cola.`

const (
	// convMaxMsgs guarda los últimos mensajes por conversación (10 turnos).
	convMaxMsgs = 20
	// convMaxConvs es el tope de conversaciones en memoria.
	convMaxConvs = 256
)

// angleRe detecta el pedido de ángulos del flujo /agency en el mensaje
// cuando el front no marcó el artefacto explícitamente (ruta legada).
var angleRe = regexp.MustCompile(`(?i)(á|a)ngulo`)

// chatMsg es un turno del historial en formato OpenAI/Anthropic.
type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// conversationKey identifica una conversación DENTRO de su tenant y
// usuario: dos tenants (o dos usuarios del mismo tenant) que manden el
// mismo conversation_id nunca comparten historial — el aislamiento no
// depende de que el cliente elija ids únicos.
type conversationKey struct {
	TenantID int64
	UserID   int64
	ConvID   string
}

// convStore guarda el historial de turnos del fallback directo en
// memoria: sin agent-server no hay persistencia entre llamadas y el
// panel de chat del front necesita continuidad de conversación.
//
// Las claves son (tenant, usuario, conversación): aislar por tenant+user
// garantiza que un conversation_id ajeno no filtre historial de otro
// flujo (regla del workspace: los flujos se mantienen aislados por
// multitenant).
type convStore struct {
	mu    sync.Mutex
	turns map[conversationKey][]chatMsg
	order []conversationKey
}

func newConvStore() *convStore {
	return &convStore{turns: make(map[conversationKey][]chatMsg)}
}

// history devuelve el historial previo de la conversación.
func (s *convStore) history(key conversationKey) []chatMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]chatMsg, len(s.turns[key]))
	copy(out, s.turns[key])
	return out
}

// append agrega los mensajes del turno y expulsa la conversación más
// vieja si se supera el tope.
func (s *convStore) append(key conversationKey, msgs ...chatMsg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.turns[key]; !ok {
		s.order = append(s.order, key)
	}
	turns := append(s.turns[key], msgs...)
	if len(turns) > convMaxMsgs {
		turns = turns[len(turns)-convMaxMsgs:]
	}
	s.turns[key] = turns
	for len(s.order) > convMaxConvs {
		delete(s.turns, s.order[0])
		s.order = s.order[1:]
	}
}

// directChat atiende un turno sin agent-server: resuelve provider/modelo,
// elige la persona experta del workflow y llama al LLM del tenant con sus
// herramientas (loop de tool-calling), emitiendo los eventos SSE que el
// front espera más los eventos "tool" del avance de cada invocación.
func (m *Module) directChat(c *gin.Context, req chatRequest, hdl *credential.Handler, cred *credential.Resolve, deps *toolkit.Deps) {
	provider, model, useCred := routeLLM(hdl, req, cred)

	// El experto del flujo /agency define el tono y el conocimiento;
	// el artefacto sales_angles solo cuando lo pide el front (o la ruta
	// legada cuyo mensaje menciona ángulos).
	// Turno de artefacto (paso del flujo /agency): contrato JSON en vez
	// de chat, con las secciones del .md del paso. La ruta legacy del
	// sales_angles (mensaje que menciona "ángulo" sin marcar el artefacto)
	// sigue cayendo en el mismo camino.
	kind := req.Artifact
	if kind == "" && angleRe.MatchString(req.Message) {
		kind = artifactSalesAngles
	}
	if spec, ok := parseArtifactKind(kind); ok {
		m.artifactTurn(c, req, hdl, cred, spec)
		return
	}

	persona := personaFor(req.Workflow)
	system := persona.ChatSystem
	if deps != nil {
		// Con herramientas disponibles el experto pasa a ser operador:
		// consulta y ejecuta sobre el tenant antes de contestar.
		system = persona.ChatSystem + toolsSystemHint
	}

	convID := convIDFor(req)
	// Historial aislado por tenant+usuario: un conversation_id repetido en
	// otro tenant (o de otro usuario) nunca cruza datos.
	convKey := conversationKey{
		TenantID: tenantIDFrom(c),
		UserID:   utils.UserIDFromContext(c),
		ConvID:   convID,
	}

	// El chat arrastra el historial para que el experto recuerde el
	// proyecto entre turnos (los artefactos son one-shot y van por
	// artifactTurn, que no usa memoria).
	msgs := append(m.convs.history(convKey), chatMsg{Role: "user", Content: req.Message})

	ctx, cancel := context.WithTimeout(c.Request.Context(), llmTimeout)
	defer cancel()

	var answer string
	var err error
	if deps == nil {
		// Sin bundle del tenant (sin tools): una llamada directa.
		answer, err = llmAnswer(ctx, m.client, provider, useCred, model, system, msgs, chatMaxTokens)
	} else {
		// Loop agéntico: el LLM consulta/genera y recién entonces
		// contesta. Cada tool emite SSE "tool" al front.
		ctx = toolkit.WithDeps(ctx, deps)
		answer, err = runToolLoop(ctx, m.client, provider, useCred, model, system, msgs, agentTools(), func(ev toolEvent) {
			sseWrite(c, "tool", ev)
		})
	}
	if err != nil {
		utils.Error(c, http.StatusBadGateway, "llm "+provider+": "+err.Error())
		return
	}
	m.convs.append(convKey, msgs...)
	m.convs.append(convKey, chatMsg{Role: "assistant", Content: answer})

	c.Status(http.StatusOK)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()

	if !sseWrite(c, "meta", map[string]string{"conversation_id": convID}) {
		return
	}
	sseWrite(c, "text", map[string]string{"text": answer})
}

// artifactTurn atiende un turno de artefacto del flujo /agency sin tools:
// arma el system prompt con las secciones del .md del paso, pide el JSON
// del contrato (con más tokens que el chat), lo valida contra el contrato
// y emite el evento "artifact". Si el artefacto pide información al
// usuario emite "questions" y fin de turno; el paso 1 inmobiliario
// mantiene además el alias legacy "sales_angles".
func (m *Module) artifactTurn(c *gin.Context, req chatRequest, hdl *credential.Handler, cred *credential.Resolve, spec artifactSpec) {
	provider, model, useCred := routeLLM(hdl, req, cred)

	system := systemFor(req.Workflow, spec.Step)
	if system == "" {
		// Sin secciones del .md embebido cae al prompt de la persona.
		system = personaFor(req.Workflow).AnglesSystem
	}

	payload := decodePayload(req.Payload)
	msgs := []chatMsg{{Role: "user", Content: artifactUserMessage(req, spec)}}

	ctx, cancel := context.WithTimeout(c.Request.Context(), artifactTimeout)
	defer cancel()

	answer, err := llmAnswer(ctx, m.client, provider, useCred, model, system, msgs, artifactMaxTokens)
	if err != nil {
		utils.Error(c, http.StatusBadGateway, "llm "+provider+": "+err.Error())
		return
	}
	data, ok := extractArtifactJSON(answer)
	if !ok {
		startSSE(c)
		sseWrite(c, "meta", map[string]string{"conversation_id": convIDFor(req)})
		sseWrite(c, "error", map[string]string{"message": "el LLM no devolvió el artefacto en JSON"})
		return
	}

	startSSE(c)
	if !sseWrite(c, "meta", map[string]string{"conversation_id": convIDFor(req)}) {
		return
	}

	// Un solo reintento de corrección si el contrato no se cumple (§6.2):
	// si persiste, el artefacto se devuelve con advertencias en vez de
	// fallar el turno.
	warns := validateArtifact(spec, data, payload)
	for attempt := 0; len(warns) > 0 && attempt < 1; attempt++ {
		msgs = append(msgs,
			chatMsg{Role: "assistant", Content: answer},
			chatMsg{Role: "user", Content: correctionMessage(warns)},
		)
		retry, retryErr := llmAnswer(ctx, m.client, provider, useCred, model, system, msgs, artifactMaxTokens)
		if retryErr != nil {
			break
		}
		fixed, fixedOK := extractArtifactJSON(retry)
		if !fixedOK {
			break
		}
		answer, data = retry, fixed
		warns = validateArtifact(spec, data, payload)
	}

	// Estado "necesita_informacion": solo preguntas al chat, sin artefacto.
	if questions := artifactQuestions(data); questions != nil {
		if len(questions) == 0 {
			// El modelo pidió información sin concretar preguntas: no se
			// cierra el turno en vacío.
			questions = []string{"El agente necesita más información para continuar: completá el brief del paso o consultá qué falta."}
		}
		sseWrite(c, "questions", map[string]any{
			"step":      spec.Step,
			"kind":      spec.Kind,
			"questions": questions,
		})
		return
	}
	if len(warns) > 0 {
		appendWarnings(data, warns)
	}

	if spec.Kind == artifactSalesAngles {
		normalizeSalesAngles(data)
		// Alias legacy: el front actual todavía escucha "sales_angles"
		// para el paso 1 inmobiliario; se retira en la Fase 3.
		sseWrite(c, "sales_angles", map[string]any{"angles": data["angles"]})
	}
	sseWrite(c, "artifact", map[string]any{
		"step": spec.Step,
		"kind": spec.Kind,
		"data": data,
	})
}

// artifactUserMessage arma el mensaje de un turno de artefacto: el
// payload del paso (brief, cantidades, referencias) como JSON legible, más
// el mensaje libre del usuario si lo hubo. Sin payload, el mensaje lleva
// la entrada completa (ruta legacy).
func artifactUserMessage(req chatRequest, spec artifactSpec) string {
	var sb strings.Builder
	if req.Message != "" {
		sb.WriteString("Mensaje del usuario: ")
		sb.WriteString(req.Message)
		sb.WriteString("\n\n")
	}
	if len(req.Payload) > 0 {
		fmt.Fprintf(&sb, "Entrada del paso %d (%s):\n", spec.Step, spec.Kind)
		var buf bytes.Buffer
		if err := json.Indent(&buf, req.Payload, "", "  "); err == nil {
			sb.Write(buf.Bytes())
		} else {
			sb.Write(req.Payload)
		}
		sb.WriteString("\n\n")
	}
	sb.WriteString("Respondé EXCLUSIVAMENTE con el JSON de la SALIDA de ese paso, sin markdown ni texto alrededor.")
	return sb.String()
}

// convIDFor devuelve el conversation_id del turno o uno local nuevo.
func convIDFor(req chatRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	return "local-" + uuid.NewString()
}

// startSSE abre las cabeceras del stream SSE de la respuesta.
func startSSE(c *gin.Context) {
	c.Status(http.StatusOK)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()
}

// routeLLM decide provider/modelo reales de la llamada. Un slug estilo
// OpenRouter ("anthropic/claude-sonnet-4.5") solo existe en OpenRouter:
// si la petición vino con otro provider, se re-ruta allí si hay key.
func routeLLM(hdl *credential.Handler, req chatRequest, cred *credential.Resolve) (string, string, *credential.Resolve) {
	provider := req.Provider
	model := strings.TrimSpace(req.Model)

	if strings.Contains(model, "/") && provider != credential.ProviderOpenRouter {
		if orCred, err := hdl.Resolve(credential.ProviderOpenRouter); err == nil && orCred != nil && orCred.AuthKey() != "" {
			provider, cred = credential.ProviderOpenRouter, orCred
		}
	}
	if model == "" {
		model = modelFromExtra(cred.Extra)
	}
	if model == "" {
		model = defaultLLMModel(provider)
	}
	// Alias del catálogo → ID real del provider.
	if provider != credential.ProviderOpenRouter && (model == "claude-text" || model == "claude") {
		model = anthropicModel
	}
	return provider, model, cred
}

// defaultLLMModel es el modelo de arranque cuando ni la petición ni la
// credencial traen uno.
func defaultLLMModel(provider string) string {
	if provider == credential.ProviderAnthropic {
		return anthropicModel
	}
	return openrouterModel
}

// llmAnswer hace una llamada chat-completion (no streaming) con el
// system prompt y el historial de mensajes, y devuelve el texto del
// asistente. maxTokens acota la respuesta (chat vs. artefacto).
func llmAnswer(ctx context.Context, client *http.Client, provider string, cred *credential.Resolve, model, system string, msgs []chatMsg, maxTokens int) (string, error) {
	if provider != credential.ProviderOpenRouter && provider != credential.ProviderAnthropic {
		return "", fmt.Errorf("provider %q sin soporte directo: usá openrouter o anthropic", provider)
	}
	anthropic := provider == credential.ProviderAnthropic

	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	if base == "" {
		if anthropic {
			base = anthropicBase
		} else {
			base = openrouterBase
		}
	}
	path := openrouterPath
	if anthropic {
		path = anthropicPath
	}
	if ep := strings.TrimSpace(cred.Endpoint); ep != "" {
		path = ep
	}

	var body []byte
	var err error
	if anthropic {
		body, err = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"system":     system,
			"messages":   msgs,
		})
	} else {
		body, err = json.Marshal(map[string]any{
			"model":       model,
			"temperature": 0.7,
			"max_tokens":  maxTokens,
			"messages":    append([]chatMsg{{Role: "system", Content: system}}, msgs...),
		})
	}
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if anthropic {
		httpReq.Header.Set("x-api-key", cred.AuthKey())
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+cred.AuthKey())
		httpReq.Header.Set("X-Title", "Synapta")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm devolvió %s: %s", resp.Status, trimForError(raw))
	}
	return parseLLMText(raw, anthropic)
}

// parseLLMText extrae el texto del asistente de la respuesta del provider
// (formato OpenAI para OpenRouter, formato Anthropic para Anthropic).
func parseLLMText(raw []byte, anthropic bool) (string, error) {
	if anthropic {
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", fmt.Errorf("respuesta ilegible: %s", trimForError(raw))
		}
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("%s", parsed.Error.Message)
		}
		var sb strings.Builder
		for _, block := range parsed.Content {
			if block.Type == "text" || block.Type == "" {
				sb.WriteString(block.Text)
			}
		}
		if strings.TrimSpace(sb.String()) == "" {
			return "", fmt.Errorf("respuesta vacía del llm")
		}
		return sb.String(), nil
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("respuesta ilegible: %s", trimForError(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("%s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("respuesta vacía del llm")
	}
	return parsed.Choices[0].Message.Content, nil
}

// tenantIDFrom lee el tenant del ctx del middleware de tenancy; 0 si no
// hubo (tests / requests internos).
func tenantIDFrom(c *gin.Context) int64 {
	if t := tenancy.FromContext(c.Request.Context()); t != nil {
		return t.ID
	}
	return 0
}

// sseWrite emite un evento SSE; false si el cliente ya se fue.
func sseWrite(c *gin.Context, name string, payload any) bool {
	data, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return false
	}
	c.Writer.Flush()
	return true
}

// strField lee un campo string tolerante de JSON arbitrario.
func strField(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// trimForError recorta la respuesta del proveedor para mensajes de error.
func trimForError(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}
