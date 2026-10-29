package infoproduct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	// defaultBrainModel is only a fallback: the front always sends the model
	// the user picked in the "Cerebro" box.
	defaultBrainModel = "google/gemini-2.5-flash"
	llmTimeout        = 5 * time.Minute
)

// usage is the token/cost accounting OpenRouter returns with a completion.
type usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// brain talks to a multimodal chat-completions endpoint (OpenRouter today;
// a direct Gemini client can implement the same call later).
type brain struct {
	http *http.Client
}

func newBrain() *brain { return &brain{http: &http.Client{Timeout: llmTimeout}} }

// normalizeModel strips the catalog prefix ("openrouter/google/gemini-..."),
// since the API only knows the bare id.
func normalizeModel(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "openrouter/")
	if name == "" {
		return defaultBrainModel
	}
	return name
}

// textPart / imagePart / videoPart / audioPart build OpenAI-style content
// parts, the shape OpenRouter accepts for multimodal input.
func textPart(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func imagePart(dataURL string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}}
}

func videoPart(dataURL string) map[string]any {
	return map[string]any{"type": "video_url", "video_url": map[string]any{"url": dataURL}}
}

func audioPart(b64, format string) map[string]any {
	return map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": b64, "format": format}}
}

// chat sends one system + one multimodal user turn and returns the text.
func (b *brain) chat(ctx context.Context, baseURL, apiKey, model, system string, parts []map[string]any, maxTokens int) (string, usage, error) {
	return b.chatWith(ctx, baseURL, apiKey, model, system, parts, maxTokens, 0.2)
}

// chatWith is chat with an explicit temperature (used by the recreate skill).
func (b *brain) chatWith(ctx context.Context, baseURL, apiKey, model, system string, parts []map[string]any, maxTokens int, temperature float64) (string, usage, error) {
	return b.chatWithOptions(ctx, baseURL, apiKey, model, system, parts, maxTokens, chatOptions{temperature: temperature})
}

// chatOptions tweaks a single call (never the shared client defaults).
type chatOptions struct {
	temperature float64
	// timeout overrides the global client timeout for this call only.
	timeout time.Duration
	// jsonMode asks the provider for a JSON object (OpenRouter
	// response_format), without pinning a strict schema.
	jsonMode bool
}

// chatWithOptions is chatWith with per-call options; chat and chatWith keep
// their previous behavior.
func (b *brain) chatWithOptions(ctx context.Context, baseURL, apiKey, model, system string, parts []map[string]any, maxTokens int, opt chatOptions) (string, usage, error) {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	payload := map[string]any{
		"model": normalizeModel(model),
		"messages": []map[string]any{
			{"role": "system", "content": system},
			{"role": "user", "content": parts},
		},
		"max_tokens":  maxTokens,
		"temperature": opt.temperature,
		"usage":       map[string]any{"include": true},
	}
	if opt.jsonMode {
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", usage{}, err
	}
	client := b.http
	if opt.timeout > 0 {
		client = &http.Client{Timeout: opt.timeout, Transport: b.http.Transport}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opt.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Title", "Synappta VideoMachine")

	resp, err := client.Do(req)
	if err != nil {
		return "", usage{}, fmt.Errorf("el cerebro no respondio: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", usage{}, fmt.Errorf("respuesta del cerebro ilegible: %w", err)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int     `json:"prompt_tokens"`
			CompletionTokens int     `json:"completion_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", usage{}, fmt.Errorf("respuesta del cerebro invalida (HTTP %d): %s", resp.StatusCode, clip(string(raw), 300))
	}
	if resp.StatusCode >= 400 || parsed.Error != nil {
		msg := clip(string(raw), 300)
		if parsed.Error != nil && parsed.Error.Message != "" {
			msg = parsed.Error.Message
		}
		return "", usage{}, fmt.Errorf("el cerebro devolvio un error (HTTP %d): %s", resp.StatusCode, msg)
	}
	if len(parsed.Choices) == 0 {
		return "", usage{}, errors.New("el cerebro devolvio una respuesta vacia")
	}
	if strings.TrimSpace(parsed.Choices[0].FinishReason) == "length" {
		return "", usage{}, errors.New("el cerebro corto la respuesta por el limite de tokens: intenta de nuevo o baja el detalle")
	}
	text := contentText(parsed.Choices[0].Message.Content)
	if strings.TrimSpace(text) == "" {
		return "", usage{}, errors.New("el cerebro devolvio una respuesta vacia")
	}
	u := usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens, CostUSD: parsed.Usage.Cost}
	return text, u, nil
}

// contentText flattens message.content, which is a string or a list of parts.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// extractJSON pulls the JSON object out of a model answer that may be wrapped
// in markdown fences or surrounded by commentary.
func extractJSON(text string) (json.RawMessage, error) {
	text = strings.TrimSpace(text)
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return nil, errors.New("la respuesta del cerebro no contiene JSON")
	}
	candidate := text[start : end+1]
	if !json.Valid([]byte(candidate)) {
		return nil, errors.New("la respuesta del cerebro no es un JSON valido")
	}
	return json.RawMessage(candidate), nil
}

// jsonAnswer asks the brain for JSON and, if the answer is not valid, runs one
// text-only repair pass (cheap: no media attached).
func (b *brain) jsonAnswer(ctx context.Context, baseURL, apiKey, model, system string, parts []map[string]any, maxTokens int) (json.RawMessage, usage, error) {
	text, u, err := b.chat(ctx, baseURL, apiKey, model, system, parts, maxTokens)
	if err != nil {
		return nil, u, err
	}
	if out, perr := extractJSON(text); perr == nil {
		return out, u, nil
	}
	repair := []map[string]any{textPart("The following answer was supposed to be ONE valid JSON object but is not valid JSON. Return ONLY the corrected JSON object, no commentary, no markdown fences.\n\n" + text)}
	text2, u2, err := b.chat(ctx, baseURL, apiKey, model, "You repair malformed JSON. Output only valid JSON.", repair, maxTokens)
	u.PromptTokens += u2.PromptTokens
	u.CompletionTokens += u2.CompletionTokens
	u.CostUSD += u2.CostUSD
	if err != nil {
		return nil, u, err
	}
	out, perr := extractJSON(text2)
	if perr != nil {
		return nil, u, perr
	}
	return out, u, nil
}
