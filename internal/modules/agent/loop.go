// loop.go: loop agéntico del asistente interno. Cuando el turno es chat
// (no el artefacto one-shot de ángulos) el LLM recibe el set de herramientas
// de tools.go y puede invocarlas en varios pasos antes de responder: consulta
// proyectos, genera videos/imágenes, hace polling del task y recién entonces
// contesta. Cada invocación emite un evento SSE "tool" para que el front
// muestre el avance en vivo (y pueda trackear la generación encolada).
//
// Soporta los dos providers con credentials directas: OpenRouter (formato
// OpenAI: tools + tool_calls + role "tool") y Anthropic (tools + bloques
// tool_use / tool_result). El loop corre con el ctx del request: si el
// cliente se va, se corta en el paso que esté.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"synapta/internal/modules/credential"
	"synapta/internal/toolkit"
)

const (
	// maxToolRounds corta el loop: protege contra un LLM que pida tools
	// en loop infinito (y contra la latencia acumulada).
	maxToolRounds = 6
	// maxToolResultLen recorta los tool results (p.ej. list_projects con
	// proyectos de descripción larga) para no inflar el contexto.
	maxToolResultLen = 8000
)

// toolCall es una invocación pedida por el LLM, normalizada entre providers.
type toolCall struct {
	ID   string          // id que el provider asignó (para el result)
	Name string          // nombre de la herramienta
	Args json.RawMessage // argumentos crudos (JSON)
}

// llmTurnState es el estado acumulado del loop para un turno.
type llmTurnState struct {
	Text      string     // texto final (o parcial) del asistente
	ToolCalls []toolCall // invocaciones pedidas en este paso
	// rawOpenAI / rawAnthropic guardan el historial nativo del provider
	// (mensajes con tool_calls / bloques tool_use) para seguir el loop.
	rawOpenAI    []map[string]any
	rawAnthropic []map[string]any
}

// runToolLoop ejecuta un turno completo con herramientas hasta que el LLM
// responde sin pedir más tools (o se alcanza maxToolRounds). onEvent recibe
// el avance de cada tool para el front; devuelve el texto final.
func runToolLoop(
	ctx context.Context,
	client *http.Client,
	provider string,
	cred *credential.Resolve,
	model, system string,
	msgs []chatMsg,
	tools []toolDef,
	onEvent func(toolEvent),
) (string, error) {
	anthropic := provider == credential.ProviderAnthropic
	if provider != credential.ProviderOpenRouter && !anthropic {
		return "", fmt.Errorf("provider %q sin soporte directo: usá openrouter o anthropic", provider)
	}

	// Historial nativo de arranque (los turnos previos van como texto plano).
	native := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		native = append(native, map[string]any{"role": m.Role, "content": m.Content})
	}

	var final strings.Builder
	for round := 0; round < maxToolRounds; round++ {
		state, err := llmStep(ctx, client, provider, cred, model, system, native, tools, anthropic)
		if err != nil {
			return "", err
		}
		if len(state.ToolCalls) == 0 {
			// Sin pedir tools: respuesta final.
			if strings.TrimSpace(state.Text) == "" {
				return "", fmt.Errorf("respuesta vacía del llm")
			}
			return state.Text, nil
		}
		if strings.TrimSpace(state.Text) != "" {
			final.WriteString(state.Text)
			final.WriteString("\n\n")
		}

		// Ejecuta cada tool pedido y devuelve los results al historial.
		results := make([]map[string]any, 0, len(state.ToolCalls))
		for _, call := range state.ToolCalls {
			result, _ := executeTool(ctx, tools, call, onEvent)
			if anthropic {
				results = append(results, map[string]any{
					"type":        "tool_result",
					"tool_use_id": call.ID,
					"content":     result,
					"is_error":    strings.HasPrefix(result, "error:"),
				})
			} else {
				results = append(results, map[string]any{
					"role":         "tool",
					"tool_call_id": call.ID,
					"content":      result,
				})
			}
		}
		if anthropic {
			// Anthropic: el assistant con tool_use y el user con tool_result
			// van como bloques de contenido.
			native = append(native, state.rawAnthropic...)
			native = append(native, map[string]any{"role": "user", "content": results})
		} else {
			native = append(native, state.rawOpenAI...)
			native = append(native, results...)
		}
	}

	return "", fmt.Errorf("el asistente no terminó en %d pasos de herramientas", maxToolRounds)
}

// executeTool corre una invocación del LLM contra el registry y serializa el
// resultado (o el error) como texto para devolverle al modelo.
func executeTool(ctx context.Context, tools []toolDef, call toolCall, onEvent func(toolEvent)) (string, toolEvent) {
	var def *toolDef
	for i := range tools {
		if tools[i].Name == call.Name {
			def = &tools[i]
			break
		}
	}
	ev := toolEvent{Name: call.Name, Status: "running"}
	if def == nil {
		ev.Status = "error"
		ev.Error = "unknown tool"
		if onEvent != nil {
			onEvent(ev)
		}
		return "error: unknown tool " + call.Name, ev
	}
	if onEvent != nil {
		onEvent(ev)
	}

	// Los tools reciben los deps por ctx (cableados en chatTurn).
	deps, _ := toolkit.DepsFrom(ctx)
	if deps == nil {
		ev.Status = "error"
		ev.Error = "tenant context missing"
		if onEvent != nil {
			onEvent(ev)
		}
		return "error: tenant context missing", ev
	}

	out, err := def.Run(ctx, deps, call.Args, onEvent)
	if err != nil {
		ev.Status = "error"
		ev.Error = err.Error()
		if onEvent != nil {
			onEvent(ev)
		}
		return "error: " + err.Error(), ev
	}
	ev.Status = "ok"
	if onEvent != nil {
		onEvent(ev)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf("error: marshal result: %v", err), ev
	}
	if len(raw) > maxToolResultLen {
		raw = append(raw[:maxToolResultLen], []byte("…(truncated)")...)
	}
	return string(raw), ev
}

// llmStep da un paso al LLM (con tools) y devuelve texto + tool calls pedidos.
func llmStep(
	ctx context.Context,
	client *http.Client,
	provider string,
	cred *credential.Resolve,
	model, system string,
	native []map[string]any,
	tools []toolDef,
	anthropic bool,
) (*llmTurnState, error) {
	base := strings.TrimRight(strings.TrimSpace(cred.BaseURL), "/")
	path := openrouterPath
	if anthropic {
		path = anthropicPath
	}
	// Sin BaseURL en la credencial cae al endpoint oficial del provider
	// (una BaseURL custom, p.ej. un gateway, manda).
	if base == "" {
		if anthropic {
			base = anthropicBase
		} else {
			base = openrouterBase
		}
	}
	if ep := strings.TrimSpace(cred.Endpoint); ep != "" {
		path = ep
	}

	var body []byte
	var err error
	if anthropic {
		body, err = json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 2048,
			"system":     system,
			"messages":   native,
			"tools":      anthropicTools(tools),
		})
	} else {
		body, err = json.Marshal(map[string]any{
			"model":       model,
			"temperature": 0.7,
			"max_tokens":  2048,
			"messages":    append([]map[string]any{{"role": "system", "content": system}}, native...),
			"tools":       openaiTools(tools),
		})
	}
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm devolvió %s: %s", resp.Status, trimForError(raw))
	}
	if anthropic {
		return parseAnthropicStep(raw)
	}
	return parseOpenAIStep(raw)
}

// openaiTools convierte el registry al formato "tools" de OpenAI.
func openaiTools(tools []toolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// anthropicTools convierte el registry al formato "tools" de Anthropic.
func anthropicTools(tools []toolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Parameters,
		})
	}
	return out
}

// parseOpenAIStep extrae texto + tool_calls de una respuesta de OpenAI.
func parseOpenAIStep(raw []byte) (*llmTurnState, error) {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name string `json:"name"`
						// Arguments viene como string con JSON adentro
						// (formato OpenAI); se decodifica abajo.
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("respuesta ilegible: %s", trimForError(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("respuesta sin choices: %s", trimForError(raw))
	}
	msg := parsed.Choices[0].Message
	state := &llmTurnState{Text: msg.Content}
	for _, tc := range msg.ToolCalls {
		state.ToolCalls = append(state.ToolCalls, toolCall{ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)})
		// Guarda el mensaje nativo para reenviarlo en el próximo paso.
		state.rawOpenAI = append(state.rawOpenAI, map[string]any{
			"role":    "assistant",
			"content": msg.Content,
			"tool_calls": []map[string]any{{
				"id":       tc.ID,
				"type":     "function",
				"function": map[string]any{"name": tc.Function.Name, "arguments": string(tc.Function.Arguments)},
			}},
		})
	}
	return state, nil
}

// parseAnthropicStep extrae texto + bloques tool_use de una respuesta.
func parseAnthropicStep(raw []byte) (*llmTurnState, error) {
	var parsed struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("respuesta ilegible: %s", trimForError(raw))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	state := &llmTurnState{}
	var blocks []map[string]any
	for _, b := range parsed.Content {
		switch b.Type {
		case "text":
			state.Text += b.Text
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case "tool_use":
			state.ToolCalls = append(state.ToolCalls, toolCall{ID: b.ID, Name: b.Name, Args: b.Input})
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": json.RawMessage(b.Input),
			})
		}
	}
	state.rawAnthropic = append(state.rawAnthropic, map[string]any{"role": "assistant", "content": blocks})
	return state, nil
}
