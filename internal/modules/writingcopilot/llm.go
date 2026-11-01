package writingcopilot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"synapta/internal/modules/credential"
)

const (
	openrouterBase = "https://openrouter.ai/api/v1"
	openrouterPath = "/chat/completions"
	anthropicBase  = "https://api.anthropic.com"
	anthropicPath  = "/v1/messages"

	// defaultModel is the project text default when the request and the env
	// leave the model empty (OpenRouter id).
	defaultModel = "openai/gpt-4o-mini"
)

// usage is the token accounting the provider returns with a completion.
type usage struct {
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// brain talks to the project's text providers: OpenRouter (API key in the
// tenant) or Anthropic directly. The wire format follows the provider.
type brain struct {
	http *http.Client
}

func newBrain() *brain { return &brain{http: &http.Client{Timeout: callTimeout + time.Minute}} }

// resolveBrain picks the provider, credential and bare model id for a request.
// Catalog names carry the provider prefix ("openrouter/anthropic/claude-...");
// a vendor-style slug ("anthropic/claude-...") belongs to OpenRouter as well.
func resolveBrain(hdl *credential.Handler, requested string) (provider string, cred *credential.Resolve, model string, err error) {
	provider, model = splitModel(requested)

	resolved, rerr := hdl.Resolve(provider)
	if rerr != nil {
		return "", nil, "", rerr
	}
	if resolved == nil || resolved.AuthKey() == "" {
		// Fall back to the other supported provider when the tenant has a key.
		other := credential.ProviderOpenRouter
		if provider == credential.ProviderOpenRouter {
			other = credential.ProviderAnthropic
		}
		alt, aerr := hdl.Resolve(other)
		if aerr == nil && alt != nil && alt.AuthKey() != "" {
			provider, resolved = other, alt
		}
	}
	if resolved == nil || resolved.AuthKey() == "" {
		return "", nil, "", errors.New("falta la API key del cerebro: configura OpenRouter o Anthropic en las credenciales del tenant")
	}
	if provider == credential.ProviderAnthropic && (model == "claude-text" || model == "claude") {
		model = "claude-haiku-4-5"
	}
	if model == "" {
		model = defaultModel
	}
	return provider, resolved, model, nil
}

// splitModel maps a catalog name to its provider and bare model id. The env
// override (LLM_MODEL_WRITING_COPILOT) wins over the request.
func splitModel(requested string) (provider, model string) {
	requested = strings.TrimSpace(requested)
	if env := getenvModel(); env != "" {
		requested = env
	}
	provider = credential.ProviderOpenRouter
	switch {
	case strings.HasPrefix(requested, credential.ProviderOpenRouter+"/"):
		model = strings.TrimPrefix(requested, credential.ProviderOpenRouter+"/")
	case strings.HasPrefix(requested, credential.ProviderAnthropic+"/"):
		provider = credential.ProviderAnthropic
		model = strings.TrimPrefix(requested, credential.ProviderAnthropic+"/")
	default:
		model = requested
	}
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	return provider, model
}

// chat runs one completion with the system prompt and the conversation turns.
func (b *brain) chat(ctx context.Context, provider string, cred *credential.Resolve, model, system string, messages []map[string]any, temperature float64, maxTokens int) (string, usage, error) {
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

	var payload map[string]any
	if anthropic {
		payload = map[string]any{
			"model":       model,
			"max_tokens":  maxTokens,
			"temperature": temperature,
			"system":      system,
			"messages":    messages,
		}
	} else {
		turns := make([]map[string]any, 0, len(messages)+1)
		turns = append(turns, map[string]any{"role": "system", "content": system})
		turns = append(turns, messages...)
		payload = map[string]any{
			"model":       model,
			"messages":    turns,
			"max_tokens":  maxTokens,
			"temperature": temperature,
			"usage":       map[string]any{"include": true},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return "", usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if anthropic {
		req.Header.Set("x-api-key", cred.AuthKey())
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+cred.AuthKey())
		req.Header.Set("X-Title", "Synappta Writing Copilot")
	}

	resp, err := b.http.Do(req)
	if err != nil {
		return "", usage{}, fmt.Errorf("el cerebro no respondio: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", usage{}, fmt.Errorf("respuesta del cerebro ilegible: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", usage{}, fmt.Errorf("el cerebro devolvio un error (HTTP %d): %s", resp.StatusCode, clip(string(raw), 300))
	}
	text, u, err := parseAnswer(raw, anthropic)
	if err != nil {
		return "", u, err
	}
	if strings.TrimSpace(text) == "" {
		return "", u, errors.New("el cerebro devolvio una respuesta vacia")
	}
	return text, u, nil
}

// parseAnswer flattens the provider response into text + usage.
func parseAnswer(raw []byte, anthropic bool) (string, usage, error) {
	if anthropic {
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason string `json:"stop_reason"`
			Usage      struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", usage{}, fmt.Errorf("respuesta del cerebro invalida: %s", clip(string(raw), 300))
		}
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", usage{}, errors.New(parsed.Error.Message)
		}
		if parsed.StopReason == "max_tokens" {
			return "", usage{}, errors.New("el cerebro corto la respuesta por el limite de tokens: pedi una version mas corta")
		}
		var sb strings.Builder
		for _, block := range parsed.Content {
			if block.Type == "text" || block.Type == "" {
				sb.WriteString(block.Text)
			}
		}
		return sb.String(), usage{PromptTokens: parsed.Usage.InputTokens, CompletionTokens: parsed.Usage.OutputTokens}, nil
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
		return "", usage{}, fmt.Errorf("respuesta del cerebro invalida: %s", clip(string(raw), 300))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", usage{}, errors.New(parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", usage{}, errors.New("el cerebro devolvio una respuesta vacia")
	}
	if parsed.Choices[0].FinishReason == "length" {
		return "", usage{}, errors.New("el cerebro corto la respuesta por el limite de tokens: pedi una version mas corta")
	}
	u := usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		CostUSD:          parsed.Usage.Cost,
	}
	return contentText(parsed.Choices[0].Message.Content), u, nil
}

// imagePart builds the provider-specific vision block for one reference image.
func imagePart(provider, mime string, data []byte) map[string]any {
	b64 := base64.StdEncoding.EncodeToString(data)
	if provider == credential.ProviderAnthropic {
		return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mime, "data": b64}}
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + b64}}
}

// contentText flattens message.content, which is a string or a list of parts.
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
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
