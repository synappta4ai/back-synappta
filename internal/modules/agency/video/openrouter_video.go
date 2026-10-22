// OpenRouter video generator.
//
// Serves OpenRouter video models (Veo, Kling, Wan, Seedance, Hailuo, …)
// through the dedicated async API: POST {base}/videos submits the job,
// GET {base}/videos/{id} polls it, and the finished clip is downloaded
// from the poll response's unsigned_urls (or the content endpoint).
//
// Model discovery (GET /videos/models with supported durations, resolutions
// and aspect ratios) lives in the model package; this generator only needs
// the entry's endpoint (/videos) and passes the provider-verified values
// through. Reference images from the request travel as input_references
// (reference-to-video).
package agencyvideo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/model"
	"synapta/internal/utils"
)

// openRouterVideoTimeout bounds a submit/poll round-trip. Generations take
// minutes; each HTTP call is fast and polling is driven by the core.
const openRouterVideoTimeout = 60 * time.Second

// openRouterVideoResolutions / Ratios mirror the values the OpenRouter Video
// API documents. Unknown values are rejected here with a clear error instead
// of failing late at the provider.
var openRouterVideoResolutions = map[string]bool{
	"480p": true, "720p": true, "768p": true, "1080p": true,
	"1K": true, "2K": true, "4K": true,
}

var openRouterVideoRatios = map[string]bool{
	"16:9": true, "9:16": true, "1:1": true, "4:3": true, "3:4": true,
	"3:2": true, "2:3": true, "21:9": true, "9:21": true,
}

// OpenRouterVideoGenerator runs async video generation through OpenRouter.
type OpenRouterVideoGenerator struct {
	httpClient *http.Client
}

// NewOpenRouterVideoGenerator builds the OpenRouter video generator.
func NewOpenRouterVideoGenerator() *OpenRouterVideoGenerator {
	return &OpenRouterVideoGenerator{
		httpClient: &http.Client{Timeout: openRouterVideoTimeout},
	}
}

// Name returns the generator name declared by discovered video entries.
func (g *OpenRouterVideoGenerator) Name() string { return model.GeneratorOpenRouterVideo }

// ContentType returns "video".
func (g *OpenRouterVideoGenerator) ContentType() string { return "video" }

// Match reports whether the model is a video model on OpenRouter.
// Other hosted providers have no video route (their video-named ids stay
// text by design), and RunPod is endpoint-scoped.
func (g *OpenRouterVideoGenerator) Match(modelName string) bool {
	provider, id, ok := model.ParseInferenceModel(modelName)
	if !ok || provider != model.ProviderOpenRouter || model.IsEndpointScopedProvider(provider) {
		return false
	}
	return model.ResolveInferenceModality(provider, id) == model.ModalityVideo
}

// Validate checks the request against the OpenRouter Video API contract.
func (g *OpenRouterVideoGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	provider, _, ok := model.ParseInferenceModel(req.Model)
	if !ok || provider != model.ProviderOpenRouter {
		errs.Add("model", "is not an OpenRouter video model (expected openrouter/<model>)")
	}
	if req.Duration < 0 {
		errs.Add("duration", "must be a positive number of seconds")
	}
	if req.Resolution != "" && !openRouterVideoResolutions[req.Resolution] {
		errs.Add("resolution", "must be one of: 480p, 720p, 768p, 1080p, 1K, 2K, 4K")
	}
	if req.Ratio != "" && !openRouterVideoRatios[req.Ratio] {
		errs.Add("ratio", "must be one of: 16:9, 9:16, 1:1, 4:3, 3:4, 3:2, 2:3, 21:9, 9:21")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// Generate submits the video job. The provider's job id becomes the task id
// (same pattern as the Higgsfield generator), so GetStatus can poll
// GET {base}/videos/{id} without extra state.
func (g *OpenRouterVideoGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	result, err := g.doRequest(req.BaseURL+req.Endpoint, http.MethodPost, payload, req.AuthKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["id"].(string)
	if taskID == "" {
		taskID, _ = result["job_id"].(string)
	}
	if taskID == "" {
		return nil, fmt.Errorf("no job id in OpenRouter response")
	}

	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  config.STATUS_RUNNING,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}, nil
}

// GetStatus polls the job and downloads the clip on completion. The core
// passes the tenant's OpenRouter key as apiKey (also required for download:
// content URLs are authorized, not public).
func (g *OpenRouterVideoGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	statusURL := strings.TrimRight(baseURL, "/") + "/videos/" + taskID
	result, err := g.doRequest(statusURL, http.MethodGet, nil, apiKey)
	if err != nil {
		return nil, err
	}

	status := openRouterVideoStatus(result)
	if status == config.STATUS_SUCCESS {
		videoURL := openRouterVideoURL(result, baseURL, taskID)
		if videoURL == "" {
			return &agency.GeneratorResult{
				TaskID: taskID, Model: "openrouter-video", Status: "succeeded_no_url",
				Outputs: []agency.OutputResource{}, Raw: result,
				Error: "Job completed but no video URL was found in the response.",
			}, nil
		}
		localName := fmt.Sprintf("openrouter_%d_%s.mp4", time.Now().UnixMilli(), taskID)
		outputs := []agency.OutputResource{{URL: videoURL, Type: "video"}}
		if local, derr := g.download(videoURL, apiKey, localName); derr == nil {
			outputs[0].LocalURL = local
		}
		genResult := &agency.GeneratorResult{
			TaskID: taskID, Model: "openrouter-video", Status: status,
			Outputs: outputs, Raw: result,
		}
		genResult.CostUSD = openRouterVideoCost(result)
		return genResult, nil
	}

	if status == config.STATUS_FAILED {
		errMsg, _ := result["error"].(string)
		if errMsg == "" {
			errMsg = "OpenRouter video generation failed"
		}
		return &agency.GeneratorResult{
			TaskID: taskID, Model: "openrouter-video", Status: status,
			Outputs: []agency.OutputResource{}, Raw: result, Error: errMsg,
		}, nil
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: "openrouter-video", Status: status,
		Outputs: []agency.OutputResource{}, Raw: result,
	}, nil
}

// CancelTask reports that the OpenRouter Video API exposes no cancellation
// route: submitted jobs run to completion (or fail) on the provider side.
func (g *OpenRouterVideoGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return fmt.Errorf("OpenRouter video jobs cannot be cancelled once submitted")
}

// BuildPayload builds the OpenRouter video submit body. The provider prefix
// is stripped: OpenRouter only knows its own slug. Reference images travel
// as input_references (reference-to-video); durations, resolution and aspect
// ratio pass through when set (the provider defaults the rest).
func (g *OpenRouterVideoGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	_, modelID, _ := model.ParseInferenceModel(req.Model)
	if modelID == "" {
		modelID = req.Model
	}
	payload := map[string]interface{}{
		"model":  modelID,
		"prompt": agency.CompileContentText(req.Content),
	}
	if req.Duration > 0 {
		payload["duration"] = req.Duration
	}
	if req.Resolution != "" {
		payload["resolution"] = req.Resolution
	}
	if req.Ratio != "" {
		payload["aspect_ratio"] = req.Ratio
	}
	if req.GenerateAudio {
		payload["generate_audio"] = true
	}
	if seed, err := strconv.Atoi(strings.TrimSpace(req.Seed)); err == nil && strings.TrimSpace(req.Seed) != "" {
		payload["seed"] = seed
	}
	refs := []map[string]interface{}{}
	for _, item := range req.Content {
		if item.Type != "image" || (item.DataURL == "" && item.ID == "") {
			continue
		}
		url := item.DataURL
		if url == "" {
			url = item.ID
		}
		refs = append(refs, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": url},
		})
	}
	if len(refs) > 0 {
		payload["input_references"] = refs
	}
	return payload
}

// openRouterVideoStatus normalizes the poll status to Synapta's.
func openRouterVideoStatus(result map[string]interface{}) string {
	raw, _ := result["status"].(string)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "completed":
		return config.STATUS_SUCCESS
	case "failed":
		return config.STATUS_FAILED
	default: // pending, in_progress (and unknowns while polling)
		return config.STATUS_RUNNING
	}
}

// openRouterVideoURL finds the finished clip: unsigned_urls[0] when present,
// else the authorized content endpoint.
func openRouterVideoURL(result map[string]interface{}, baseURL, taskID string) string {
	if urls, ok := result["unsigned_urls"].([]interface{}); ok {
		for _, raw := range urls {
			if u, ok := raw.(string); ok && strings.TrimSpace(u) != "" {
				return strings.TrimSpace(u)
			}
		}
	}
	return strings.TrimRight(baseURL, "/") + "/videos/" + taskID + "/content"
}

// openRouterVideoCost reads the billed cost from the poll response usage.
func openRouterVideoCost(result map[string]interface{}) float64 {
	usage, ok := result["usage"].(map[string]interface{})
	if !ok {
		return 0
	}
	if cost, ok := usage["cost"].(float64); ok {
		return cost
	}
	return 0
}

// download fetches the clip with the tenant's key and stores it under the
// server outputs (published to the tenant bucket like other providers).
func (g *OpenRouterVideoGenerator) download(videoURL, apiKey, localName string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, videoURL, nil)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("video download: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return "", err
	}
	return utils.SaveBinaryOutput(data, localName)
}

// truncateVideoBody bounds an unparseable body so the error stays readable.
func truncateVideoBody(raw []byte) string {
	const max = 400
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}

// doRequest sends a JSON request to the OpenRouter API and decodes the answer.
func (g *OpenRouterVideoGenerator) doRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal video payload: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create video request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("video request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read video response: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("video response parse error: %s", truncateVideoBody(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("video API error %d: %s", resp.StatusCode, agency.ExtractError(result, string(raw)))
	}
	return result, nil
}
