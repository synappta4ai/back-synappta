// Claude text generator (Anthropic, synchronous). Wires the claude-text
// model into the agency pipeline.
package agencytext

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"synapta/internal/modules/agency"
)

// claudeModelName is the catalog model name for text generation.
const claudeModelName = "claude-text"

// ─── Generator ──────────────────────────────────────────────────

// ClaudeTextGenerator implements text generation through Anthropic Claude.
type ClaudeTextGenerator struct {
	httpClient *http.Client
}

// NewClaudeTextGenerator builds the Claude text generator.
func NewClaudeTextGenerator() *ClaudeTextGenerator {
	return &ClaudeTextGenerator{httpClient: &http.Client{Timeout: 120 * time.Second}}
}

// Name returns the generator name.
func (g *ClaudeTextGenerator) Name() string { return "claude-text" }

// ContentType returns "text".
func (g *ClaudeTextGenerator) ContentType() string { return "text" }

// Match reports whether this generator handles the model name.
func (g *ClaudeTextGenerator) Match(modelName string) bool {
	lower := strings.ToLower(modelName)
	return strings.Contains(lower, "claude")
}

// Validate checks the request against text-generation constraints.
func (g *ClaudeTextGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
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

// BuildPayload converts the request into the Claude API payload.
func (g *ClaudeTextGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	messages := buildMessages(req)
	if len(messages) == 0 {
		messages = []map[string]interface{}{
			{"role": "user", "content": agency.CompileContentText(req.Content)},
		}
	}

	// Map internal model name to a real Anthropic model ID.
	apiModel := req.Model
	switch req.Model {
	case "claude-text", "claude":
		apiModel = "claude-3-haiku-20240307"
	}

	return map[string]interface{}{
		"model":      apiModel,
		"max_tokens": 4096,
		"messages":   messages,
	}
}

func buildMessages(req *agency.GeneratorRequest) []map[string]interface{} {
	var msgs []map[string]interface{}
	foundSystem := false

	for _, item := range req.Content {
		if item.Type != "text" {
			continue
		}
		trimmed := item.Text

		// Heuristic: a leading "[SYSTEM" marker marks the system prompt.
		if !foundSystem && len(trimmed) > 7 && trimmed[:7] == "[SYSTEM" {
			msgs = append(msgs, map[string]interface{}{"role": "system", "content": trimmed})
			foundSystem = true
			continue
		}
		msgs = append(msgs, map[string]interface{}{"role": "user", "content": trimmed})
	}
	return msgs
}

// Generate sends the request to the Claude API and returns the text result.
func (g *ClaudeTextGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	result, err := g.claudeRequest(req.BaseURL+req.Endpoint, "POST", payload, req.APIKey)
	if err != nil {
		return nil, err
	}

	taskID := extractTaskID(result)
	text := extractRawText(result)

	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  "succeeded",
		Outputs: []agency.OutputResource{newTextOutput(text)},
		Raw:     result,
	}, nil
}

// GetStatus polls the Claude API for completion state.
func (g *ClaudeTextGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	url := fmt.Sprintf("%s%s/messages/%s", baseURL, endpoint, taskID)
	result, err := g.claudeRequest(url, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}
	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   claudeModelName,
		Status:  mapStatus(result),
		Outputs: []agency.OutputResource{newTextOutput(extractRawText(result))},
		Raw:     result,
	}, nil
}

// CancelTask is a no-op for synchronous text generators.
func (g *ClaudeTextGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return nil
}

func (g *ClaudeTextGenerator) claudeRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
	var reqBody []byte
	var err error
	if body != nil {
		reqBody, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal Claude payload: %w", err)
		}
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create Claude request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Claude request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Claude response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("Claude response parse error: %s", string(respBytes))
	}

	if resp.StatusCode >= 400 {
		msg := agency.ExtractError(result, string(respBytes))
		return nil, fmt.Errorf("Claude API error %d: %s", resp.StatusCode, msg)
	}
	return result, nil
}

func extractTaskID(result map[string]interface{}) string {
	if id, ok := result["id"].(string); ok && id != "" {
		return id
	}
	return fmt.Sprintf("claude_text_%d", time.Now().UnixMilli())
}

func extractRawText(result map[string]interface{}) string {
	content, ok := result["content"].([]interface{})
	if !ok {
		return ""
	}
	for _, block := range content {
		if blockMap, ok := block.(map[string]interface{}); ok {
			if blockMap["type"] == "text" {
				if text, ok := blockMap["text"].(string); ok {
					return text
				}
			}
		}
	}
	return ""
}

func mapStatus(result map[string]interface{}) string {
	if stopReason, ok := result["stop_reason"].(string); ok && stopReason != "" {
		switch stopReason {
		case "end_turn", "stop_sequence", "max_tokens":
			return "succeeded"
		case "error":
			return "failed"
		}
	}
	return "succeeded"
}

// newTextOutput encodes text as a base64 data URL over the URL field.
func newTextOutput(text string) agency.OutputResource {
	if text == "" {
		return agency.OutputResource{}
	}
	return agency.OutputResource{
		URL:  fmt.Sprintf("data:text/plain;base64,%s", base64.StdEncoding.EncodeToString([]byte(text))),
		Type: "text",
	}
}
