// Seedream 4 Pro generator (BytePlus, synchronous). Wires the
// dreamina-seedream-4-pro image model into the agency pipeline.
package agencyimage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
)

// ─── Seedream (BytePlus, synchronous) ───────────────────────────

// SeedreamGenerator runs BytePlus Seedream image generation.
type SeedreamGenerator struct {
	httpClient *http.Client
}

// NewSeedreamGenerator builds the Seedream generator.
func NewSeedreamGenerator() *SeedreamGenerator {
	return &SeedreamGenerator{httpClient: &http.Client{Timeout: 120 * time.Second}}
}

// Name returns the generator name.
func (g *SeedreamGenerator) Name() string { return "seedream" }

// ContentType returns "image".
func (g *SeedreamGenerator) ContentType() string { return "image" }

// Match reports whether this generator handles the model name.
func (g *SeedreamGenerator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "seedream")
}

// Validate checks the request against Seedream constraints.
func (g *SeedreamGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if req.Resolution != "" && !ValidResolutionsImage[req.Resolution] {
		errs.Add("resolution", "must be one of: 2K, 1080p, 720p")
	}
	if req.Duration > 0 {
		errs.Add("duration", "not supported for image generation")
	}
	if req.CameraFixed {
		errs.Add("camerafixed", "not supported for image generation")
	}
	if req.GenerateAudio {
		errs.Add("generate_audio", "not supported for image generation")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// BuildPayload builds the BytePlus image-generation payload.
func (g *SeedreamGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	content := make([]map[string]interface{}, 0)
	imageIndex := 0

	for _, item := range req.Content {
		if item.Type == "image" && item.DataURL != "" {
			content = append(content, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]string{"url": item.DataURL},
				"role":      "reference_image",
			})
			imageIndex++
		}
	}

	textPart := agency.CompileContentText(req.Content)
	if imageIndex > 0 {
		textPart = "The image references Image 1. " + textPart
	}
	content = append(content, map[string]interface{}{"type": "text", "text": textPart})

	payload := map[string]interface{}{
		"model":   req.Model,
		"content": content,
	}
	if req.Ratio != "" {
		payload["ratio"] = req.Ratio
	}
	if req.Resolution != "" {
		payload["resolution"] = req.Resolution
	}
	if req.Watermark {
		payload["watermark"] = true
	}
	if req.Seed != "" {
		payload["seed"] = req.Seed
	}
	return payload
}

// Generate runs the synchronous image generation.
func (g *SeedreamGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	result, err := g.arkRequest(req.BaseURL+req.Endpoint+"/images/generations", "POST", payload, req.APIKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["id"].(string)
	if taskID == "" {
		taskID, _ = result["task_id"].(string)
	}
	if taskID == "" {
		taskID = fmt.Sprintf("seedream_%d", time.Now().UnixMilli())
	}

	var outputs []agency.OutputResource
	if data, ok := result["data"].([]interface{}); ok {
		for _, d := range data {
			if entry, ok := d.(map[string]interface{}); ok {
				if url, ok := entry["url"].(string); ok && url != "" {
					outputs = append(outputs, agency.OutputResource{URL: url, Type: "image"})
				}
			}
		}
	}
	if len(outputs) == 0 {
		for _, key := range []string{"url", "image_url"} {
			if s, ok := result[key].(string); ok && s != "" {
				outputs = append(outputs, agency.OutputResource{URL: s, Type: "image"})
				break
			}
		}
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: req.Model, Status: config.STATUS_SUCCESS,
		Outputs: outputs, Raw: result,
	}, nil
}

// GetStatus polls a (previously async) image task.
func (g *SeedreamGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	result, err := g.arkRequest(baseURL+endpoint+"/"+taskID, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}
	status, _ := result["status"].(string)
	if status == "" {
		status = config.STATUS_SUCCESS
	}
	outputs := []agency.OutputResource{}
	if url := findImageURL(result, 6); url != "" {
		outputs = append(outputs, agency.OutputResource{URL: url, Type: "image"})
	}
	return &agency.GeneratorResult{
		TaskID: taskID, Model: "seedream", Status: status, Outputs: outputs, Raw: result,
	}, nil
}

// CancelTask is a no-op for synchronous image generation.
func (g *SeedreamGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return nil
}

func (g *SeedreamGenerator) arkRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal body: %w", err)
		}
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("seedream: %s", string(respBytes))
	}

	if resp.StatusCode >= 400 {
		msg := agency.ExtractError(result, string(respBytes))
		return nil, fmt.Errorf("seedream %d: %s", resp.StatusCode, msg)
	}
	return result, nil
}

func findImageURL(obj interface{}, maxDepth int) string {
	if obj == nil || maxDepth < 0 {
		return ""
	}
	switch v := obj.(type) {
	case []interface{}:
		for _, item := range v {
			if found := findImageURL(item, maxDepth-1); found != "" {
				return found
			}
		}
	case map[string]interface{}:
		for _, k := range []string{"image_url", "imageUrl", "url"} {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		for _, val := range v {
			if found := findImageURL(val, maxDepth-1); found != "" {
				return found
			}
		}
	}
	return ""
}
