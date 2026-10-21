// OpenAI-compatible text generator.
//
// Serves the tenant's own local LLM servers — Ollama (default :11434/v1) and
// LM Studio (default :1234/v1), plus any OpenAI-compatible endpoint the tenant
// points a local credential at. Generation is synchronous and costs nothing:
// the compute runs on the tenant's machine, so no cost calculator reports
// spend for these models.
package agencytext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
)

// openAITextGeneratorName is the generator name declared by the discovered
// catalog entries (see model.DiscoverLocalModels).
const openAITextGeneratorName = "openai-text"

// localTextTimeout is generous on purpose: a 7B model on CPU answers far
// slower than a hosted API, and cutting the request short loses the whole
// generation (the pipeline submits synchronously).
const localTextTimeout = 180 * time.Second

// OpenAITextGenerator implements text generation against an OpenAI-compatible
// /chat/completions endpoint run by the tenant.
type OpenAITextGenerator struct {
	httpClient *http.Client
}

// NewOpenAITextGenerator builds the local text generator.
func NewOpenAITextGenerator() *OpenAITextGenerator {
	return &OpenAITextGenerator{httpClient: &http.Client{Timeout: localTextTimeout}}
}

// Name returns the generator name.
func (g *OpenAITextGenerator) Name() string { return openAITextGeneratorName }

// ContentType returns "text".
func (g *OpenAITextGenerator) ContentType() string { return "text" }

// Match reports whether the model name is served by a local provider
// ("ollama/…" or "lmstudio/…"). Prefix matching is what routes the request:
// the pipeline only hands generators a name, not a provider.
func (g *OpenAITextGenerator) Match(modelName string) bool {
	_, _, ok := model.ParseLocalModel(modelName)
	return ok
}

// Validate checks the request against text-generation constraints.
func (g *OpenAITextGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if _, _, ok := model.ParseLocalModel(req.Model); !ok {
		errs.Add("model", "is not a local model (expected ollama/<model> or lmstudio/<model>)")
	}
	if req.Resolution != "" {
		errs.Add("resolution", "text generation does not support resolution overrides")
	}
	if req.Duration > 0 {
		errs.Add("duration", "text generation does not support duration")
	}
	if req.CameraFixed {
		errs.Add("camerafixed", "text generation does not support camera fixed")
	}
	if req.GenerateAudio {
		errs.Add("generate_audio", "text generation does not support audio generation")
	}
	if req.Watermark {
		errs.Add("watermark", "text generation does not support watermark")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// BuildPayload converts the request into an OpenAI chat-completions payload.
// The provider prefix is stripped: the server only knows the bare model id.
func (g *OpenAITextGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	_, modelID, _ := model.ParseLocalModel(req.Model)
	if modelID == "" {
		modelID = req.Model
	}

	messages := buildMessages(req)
	if len(messages) == 0 {
		messages = []map[string]interface{}{
			{"role": "user", "content": agency.CompileContentText(req.Content)},
		}
	}

	return map[string]interface{}{
		"model":       modelID,
		"messages":    messages,
		"max_tokens":  4096,
		"temperature": 0.7,
		"stream":      false,
	}
}

// Generate sends the request to the local server and returns the text result.
func (g *OpenAITextGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	// A bare host ("http://localhost:11434") gets /v1 appended: both Ollama and
	// LM Studio serve the OpenAI routes only under /v1.
	base := model.NormalizeLocalBaseURL(req.BaseURL)
	if base == "" {
		return nil, fmt.Errorf("local text provider %q has no base URL configured", req.Model)
	}
	path := req.Endpoint
	if strings.TrimSpace(path) == "" {
		path = "/chat/completions"
	}
	endpoint := base + path

	result, err := g.request(endpoint, payload, req.AuthKey)
	if err != nil {
		return nil, err
	}

	text := extractChatText(result)
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%s returned an empty completion", req.Model)
	}

	return &agency.GeneratorResult{
		TaskID:  fmt.Sprintf("local_text_%d", time.Now().UnixNano()),
		Model:   req.Model,
		Status:  "succeeded",
		Outputs: []agency.OutputResource{newTextOutput(text)},
		Raw:     result,
	}, nil
}

// GetStatus is a no-op: local text generation is synchronous, so a task is
// already terminal when Generate returns and the core answers polls from its
// own task record.
func (g *OpenAITextGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	return &agency.GeneratorResult{
		TaskID: taskID,
		Status: "succeeded",
		Raw:    map[string]interface{}{"note": "local generation is synchronous"},
	}, nil
}

// CancelTask is a no-op for synchronous text generators.
func (g *OpenAITextGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return nil
}

// request POSTs the payload and decodes the JSON answer. The Authorization
// header is only sent when the tenant configured a key: Ollama and LM Studio
// need none, and an empty Bearer would be rejected by stricter gateways.
func (g *OpenAITextGenerator) request(endpoint string, payload map[string]interface{}, apiKey string) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal local LLM payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create local LLM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("local LLM request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read local LLM response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("local LLM response parse error: %s", truncate(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("local LLM error %d: %s", resp.StatusCode, agency.ExtractError(result, string(raw)))
	}
	return result, nil
}

// extractChatText reads choices[0].message.content from an OpenAI-style
// response.
func extractChatText(result map[string]interface{}) string {
	choices, ok := result["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok {
		return ""
	}
	text, _ := message["content"].(string)
	return text
}

// truncate bounds an unparseable body so the error stays readable in logs.
func truncate(raw []byte) string {
	const max = 400
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}
