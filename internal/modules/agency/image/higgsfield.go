// Higgsfield image generator. Wires Higgsfield image models (Soul 2,
// Ideogram 4.0, Recraft 4.1) into the agency pipeline. All three share the
// async Higgsfield request lifecycle: POST the model endpoint, poll
// /requests/:id/status, download images[].url on completion.
// API shape per docs.higgsfield.ai (Endpoint IDs verified 2026-09):
//   - Soul 2:          POST /higgsfield-ai/soul/v2/standard  (num_images, 2K/4K)
//   - Ideogram 4.0:    POST /ideogram/v4.0                   (image_url singular for edit)
//   - Recraft V4.1:    POST /recraft/v4.1/text-to-image      (t2i only, 1k)
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

// HiggsfieldStatuses maps Higgsfield task states to Synapta statuses.
var HiggsfieldStatuses = map[string]string{
	"queued":     config.STATUS_RUNNING,
	"in_queue":   config.STATUS_RUNNING,
	"processing": config.STATUS_RUNNING,
	"in_progress": config.STATUS_RUNNING,
	"running":    config.STATUS_RUNNING,
	"completed":  config.STATUS_SUCCESS,
	"succeeded":  config.STATUS_SUCCESS,
	"failed":     config.STATUS_FAILED,
	"error":      config.STATUS_FAILED,
	"cancelled":  config.STATUS_FAILED,
	"canceled":   config.STATUS_FAILED,
	"nsfw":       config.STATUS_FAILED,
}

// Resolutions accepted per family (catalog defaults drive the UI; these
// backstop the API validation with a clear error).
var higgsfieldImageResolutions = map[string]bool{
	"1k": true, "1K": true, "2k": true, "2K": true, "4k": true, "4K": true,
}

// HiggsfieldImageGenerator runs async image generation through Higgsfield.
type HiggsfieldImageGenerator struct {
	httpClient *http.Client
	label      string
}

// NewHiggsfieldImageGenerator builds the Higgsfield image generator.
func NewHiggsfieldImageGenerator() *HiggsfieldImageGenerator {
	return &HiggsfieldImageGenerator{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		label:      "higgsfield",
	}
}

// Name returns the generator name.
func (g *HiggsfieldImageGenerator) Name() string { return g.label }

// ContentType returns "image".
func (g *HiggsfieldImageGenerator) ContentType() string { return "image" }

// Match reports whether this generator handles the model name. Video models
// are handled by agencyvideo.NewHiggsfieldGenerator.
func (g *HiggsfieldImageGenerator) Match(modelName string) bool {
	lower := strings.ToLower(modelName)
	return strings.Contains(lower, "higgsfield-") &&
		(strings.Contains(lower, "soul") ||
			strings.Contains(lower, "ideogram") ||
			strings.Contains(lower, "recraft"))
}

// Validate checks the request against Higgsfield image constraints.
func (g *HiggsfieldImageGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if req.Duration > 0 {
		errs.Add("duration", "not supported for image generation")
	}
	if req.Resolution != "" && !higgsfieldImageResolutions[req.Resolution] {
		errs.Add("resolution", "must be one of: 1k, 2K, 4K")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// Generate submits the async image task.
func (g *HiggsfieldImageGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	result, err := g.doRequest(req.BaseURL+req.Endpoint, "POST", payload, req.AuthKey)
	if err != nil {
		return nil, err
	}

	taskID := extractRequestID(result)
	if taskID == "" {
		return nil, fmt.Errorf("no request_id in response")
	}

	genResult := &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  config.STATUS_RUNNING,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}
	// Best-effort spend estimate with the exact submitted payload:
	// successful generations are billed in credits, failures are refunded.
	genResult.CostCredits, genResult.CostUSD, _ = agency.EstimateHiggsfield(
		g.httpClient, req.BaseURL, req.Endpoint, req.AuthKey, payload)

	return genResult, nil
}

// GetStatus polls the async request and collects image URLs on success.
// The core passes creds.AuthKey() as apiKey (Higgsfield needs id:secret).
func (g *HiggsfieldImageGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	statusURL := strings.TrimSuffix(baseURL, "/") + "/requests/" + taskID + "/status"
	result, err := g.doRequest(statusURL, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}

	rawStatus, _ := result["status"].(string)
	status := HiggsfieldStatus(rawStatus)

	if status == config.STATUS_SUCCESS {
		outputs := collectImageOutputs(result)
		if len(outputs) == 0 {
			return &agency.GeneratorResult{
				TaskID: taskID, Model: "higgsfield", Status: "succeeded_no_url",
				Outputs: []agency.OutputResource{}, Raw: result,
				Error: "Job completed but no image URL was found in the response.",
			}, nil
		}
		return &agency.GeneratorResult{
			TaskID: taskID, Model: "higgsfield", Status: status,
			Outputs: outputs, Raw: result,
		}, nil
	}

	if status == config.STATUS_FAILED {
		return &agency.GeneratorResult{
			TaskID: taskID, Model: "higgsfield", Status: status,
			Outputs: []agency.OutputResource{}, Raw: result,
			Error: agency.ExtractError(result, ""),
		}, nil
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: "higgsfield", Status: status,
		Outputs: []agency.OutputResource{}, Raw: result,
	}, nil
}

// CancelTask cancels the async request via its cancel route.
func (g *HiggsfieldImageGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	cancelURL := strings.TrimSuffix(baseURL, "/") + "/requests/" + taskID + "/cancel"
	_, err := g.doRequest(cancelURL, "POST", nil, apiKey)
	return err
}

// BuildPayload builds the flat Higgsfield JSON body for the endpoint.
// All three families take prompt + aspect_ratio; strict schemas
// (additionalProperties:false) mean only documented fields are sent:
//   - Soul 2: num_images when the caller asks for a batch, resolution as-is.
//   - Ideogram: image_url (singular) for edit/remix from reference images.
//   - Recraft: resolution normalized to lowercase ("1k").
func (g *HiggsfieldImageGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	payload := map[string]interface{}{
		"prompt": agency.CompileContentText(req.Content),
	}
	lowerModel := strings.ToLower(req.Model)

	if req.Ratio != "" {
		payload["aspect_ratio"] = req.Ratio
	}

	switch {
	case strings.Contains(lowerModel, "soul"):
		if req.Resolution != "" {
			payload["resolution"] = req.Resolution
		}
	case strings.Contains(lowerModel, "ideogram"):
		// Reference image → edit/remix flow (image_url singular).
		for _, item := range req.Content {
			if item.Type == "image" && item.DataURL != "" {
				payload["image_url"] = item.DataURL
				break
			}
		}
	case strings.Contains(lowerModel, "recraft"):
		// Recraft V4.1 only accepts the lowercase "1k" tier.
		if req.Resolution != "" {
			payload["resolution"] = strings.ToLower(req.Resolution)
		}
	}
	return payload
}

// HiggsfieldStatus normalizes a Higgsfield status string to Synapta's.
func HiggsfieldStatus(raw string) string {
	if mapped, ok := HiggsfieldStatuses[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return mapped
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// extractRequestID pulls the task identifier from a submit response.
func extractRequestID(result map[string]interface{}) string {
	for _, key := range []string{"request_id", "id", "task_id"} {
		if s, ok := result[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// collectImageOutputs gathers images[].url (Higgsfield image shape) with a
// recursive url fallback for provider variance.
func collectImageOutputs(result map[string]interface{}) []agency.OutputResource {
	outputs := []agency.OutputResource{}
	if images, ok := result["images"].([]interface{}); ok {
		for _, img := range images {
			if entry, ok := img.(map[string]interface{}); ok {
				if url, ok := entry["url"].(string); ok && url != "" {
					outputs = append(outputs, agency.OutputResource{URL: url, Type: "image"})
				}
			}
		}
	}
	if len(outputs) == 0 {
		if url := findImageURL(result, 6); url != "" {
			outputs = append(outputs, agency.OutputResource{URL: url, Type: "image"})
		}
	}
	return outputs
}

// doRequest sends a request to the Higgsfield API with Key auth.
func (g *HiggsfieldImageGenerator) doRequest(url, method string, body interface{}, authKey string) (map[string]interface{}, error) {
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
	req.Header.Set("Authorization", "Key "+authKey)
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
		return nil, fmt.Errorf("higgsfield: %s", string(respBytes))
	}

	if resp.StatusCode >= 400 {
		msg := agency.ExtractError(result, string(respBytes))
		return nil, fmt.Errorf("higgsfield %d: %s", resp.StatusCode, msg)
	}
	return result, nil
}
