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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/modules/credential"
	"synapta/internal/utils"
)

const (
	// llmTimeout mata la llamada antes de que el front abortue a los 45s.
	llmTimeout = 40 * time.Second

	openrouterBase  = "https://openrouter.ai/api/v1"
	openrouterPath  = "/chat/completions"
	openrouterModel = "openai/gpt-4o-mini"
	anthropicBase   = "https://api.anthropic.com"
	anthropicPath   = "/v1/messages"
	anthropicModel  = "claude-3-haiku-20240307"
)

const (
	// anglesSystemPrompt exige el artefacto sales_angles en JSON puro.
	anglesSystemPrompt = `Sos el generador de ángulos de venta de Synapta para emprendimientos inmobiliarios.
Respondé EXCLUSIVAMENTE con JSON válido, sin markdown y sin texto fuera del JSON:
{"angles":[{"title":"...","hook":"...","narrative_pitch":"...","target_audience":"...","selected":false}]}
Respetá exactamente la cantidad de ángulos que el mensaje pide.`

	chatSystemPrompt = `Sos el asistente inmobiliario de Synapta. Respondé en español, con tono profesional y conciso.`
)

// angleRe detecta el pedido de ángulos del flujo /agency en el mensaje.
var angleRe = regexp.MustCompile(`(?i)(á|a)ngulo`)

// directChat atiende un turno sin agent-server: resuelve provider/modelo,
// llama al LLM del tenant y responde con los mismos eventos SSE.
func (m *Module) directChat(c *gin.Context, req chatRequest, hdl *credential.Handler, cred *credential.Resolve) {
	provider, model, useCred := routeLLM(hdl, req, cred)

	system := chatSystemPrompt
	wantsAngles := angleRe.MatchString(req.Message)
	if wantsAngles {
		system = anglesSystemPrompt
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), llmTimeout)
	defer cancel()

	answer, err := llmAnswer(ctx, m.client, provider, useCred, model, system, req.Message)
	if err != nil {
		utils.Error(c, http.StatusBadGateway, "llm "+provider+": "+err.Error())
		return
	}

	convID := req.ConversationID
	if convID == "" {
		convID = "local-" + uuid.NewString()
	}

	c.Status(http.StatusOK)
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Flush()

	if !sseWrite(c, "meta", map[string]string{"conversation_id": convID}) {
		return
	}
	if !wantsAngles {
		sseWrite(c, "text", map[string]string{"text": answer})
		return
	}
	angles, ok := extractAngles(answer)
	if !ok {
		sseWrite(c, "error", map[string]string{"message": "el LLM no devolvió ángulos en JSON"})
		return
	}
	sseWrite(c, "sales_angles", map[string]any{"angles": angles})
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

// llmAnswer hace una llamada chat-completion (no streaming) y devuelve el
// texto del asistente.
func llmAnswer(ctx context.Context, client *http.Client, provider string, cred *credential.Resolve, model, system, user string) (string, error) {
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

	type chatMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var body []byte
	var err error
	if anthropic {
		body, err = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 2048,
			"system":     system,
			"messages":   []chatMsg{{Role: "user", Content: user}},
		})
	} else {
		body, err = json.Marshal(map[string]any{
			"model":       model,
			"temperature": 0.7,
			"max_tokens":  2048,
			"messages": []chatMsg{
				{Role: "system", Content: system},
				{Role: "user", Content: user},
			},
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

// extractAngles saca {"angles":[…]} de la respuesta del LLM, tolerando
// fences markdown y texto alrededor.
func extractAngles(raw string) ([]map[string]any, bool) {
	s := strings.TrimSpace(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, false
	}
	var parsed struct {
		Angles []map[string]any `json:"angles"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &parsed); err != nil || len(parsed.Angles) == 0 {
		return nil, false
	}

	out := make([]map[string]any, 0, len(parsed.Angles))
	for i, a := range parsed.Angles {
		title := strField(a, "title")
		if title == "" {
			title = fmt.Sprintf("Ángulo %d", i+1)
		}
		hook, pitch, audience := strField(a, "hook"), strField(a, "narrative_pitch"), strField(a, "target_audience")
		if hook == "" && pitch == "" && audience == "" {
			// Algunos modelos devuelven un solo campo "description".
			hook = strField(a, "description")
		}
		out = append(out, map[string]any{
			"title":           title,
			"hook":            hook,
			"narrative_pitch": pitch,
			"target_audience": audience,
			"selected":        a["selected"] == true,
		})
	}
	return out, true
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
