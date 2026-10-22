// OpenAI-compatible image generator.
//
// Serves the hosted inference clouds whose image route follows the OpenAI
// Images contract (Together AI, DeepInfra, OpenRouter): POST
// {base_url}{images_path} with prompt/model/size, then read
// data[].url or data[].b64_json. Providers that need a deployment-specific
// URL (Baseten) or expose a proprietary queue (GMI) are configured as text
// only, so nothing here assumes a shared path beyond the provider registry
// (model.InferenceImagePath).
package agencyimage

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
)

// openAIImageTimeout is generous: hosted FLUX/SDXL endpoints queue jobs and
// the pipeline submits synchronously.
const openAIImageTimeout = 180 * time.Second

// OpenAIImageGenerator runs image generation through an OpenAI-compatible
// images route.
type OpenAIImageGenerator struct {
	httpClient *http.Client
}

// NewOpenAIImageGenerator builds the hosted image generator.
func NewOpenAIImageGenerator() *OpenAIImageGenerator {
	return &OpenAIImageGenerator{httpClient: &http.Client{Timeout: openAIImageTimeout}}
}

// Name returns the generator name declared by discovered entries.
func (g *OpenAIImageGenerator) Name() string { return model.GeneratorOpenAIImage }

// ContentType returns "image".
func (g *OpenAIImageGenerator) ContentType() string { return "image" }

// Match reports whether the model is an image model on a hosted provider with
// an OpenAI-compatible images route. RunPod is handled by its own generator.
func (g *OpenAIImageGenerator) Match(modelName string) bool {
	provider, id, ok := model.ParseInferenceModel(modelName)
	if !ok || model.IsEndpointScopedProvider(provider) {
		return false
	}
	return model.ClassifyInferenceModel(id) == model.ModalityImage &&
		model.InferenceImagePath(provider) != ""
}

// Validate checks the request against the images API contract.
func (g *OpenAIImageGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if _, _, ok := model.ParseInferenceModel(req.Model); !ok {
		errs.Add("model", "is not a hosted inference model (expected <provider>/<model>)")
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
	if req.Resolution != "" && !ValidResolutionsImage[req.Resolution] {
		errs.Add("resolution", "must be one of: 480p, 720p, 1080p, 1K, 2K, 4K")
	}
	for _, item := range req.Content {
		if item.Type == "image" && item.DataURL != "" {
			// The shared OpenAI images route has no documented reference
			// image field; sending one would be silently dropped by some
			// providers. Fail loudly instead of ignoring the user's input.
			errs.Add("content", "reference images are not supported by this provider's image route yet")
			break
		}
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// BuildPayload builds the OpenAI images payload. The provider prefix is
// stripped from the model id: the provider only knows its own slug.
//
// response_format is intentionally omitted: DeepInfra and OpenRouter answer
// b64_json, Together answers url by default, and not every provider accepts
// the field. Generate reads both shapes.
//
// OpenRouter's Image API additionally understands resolution tiers and
// aspect_ratio, so they are sent for that provider only: other clouds
// validate strictly and would reject unknown fields.
func (g *OpenAIImageGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	provider, modelID, _ := model.ParseInferenceModel(req.Model)
	if modelID == "" {
		modelID = req.Model
	}

	payload := map[string]interface{}{
		"model":  modelID,
		"prompt": agency.CompileContentText(req.Content),
		"size":   ImageSizeForRatio(req.Ratio),
	}
	if provider == model.ProviderOpenRouter {
		if req.Ratio != "" {
			payload["aspect_ratio"] = req.Ratio
		}
		switch req.Resolution {
		case "1K", "2K", "4K":
			payload["resolution"] = req.Resolution
		}
	}
	if req.Quantity > 1 {
		payload["n"] = req.Quantity
	}
	if seed, err := strconv.Atoi(strings.TrimSpace(req.Seed)); err == nil {
		payload["seed"] = seed
	}
	return payload
}

// Generate submits the image request and collects URLs or base64 payloads.
func (g *OpenAIImageGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	path := strings.TrimSpace(req.Endpoint)
	if path == "" {
		provider, _, _ := model.ParseInferenceModel(req.Model)
		path = model.InferenceImagePath(provider)
	}
	if path == "" {
		return nil, fmt.Errorf("no OpenAI-compatible image route for model %q", req.Model)
	}
	endpoint := strings.TrimRight(req.BaseURL, "/") + path

	result, err := g.request(endpoint, payload, req.AuthKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["id"].(string)
	if taskID == "" {
		taskID = fmt.Sprintf("openai_image_%d", time.Now().UnixMilli())
	}

	outputs, err := g.collectOutputs(taskID, result)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("%s returned no image data", req.Model)
	}

	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  config.STATUS_SUCCESS,
		Outputs: outputs,
		Raw:     result,
	}, nil
}

// GetStatus is a no-op: the images route is synchronous.
func (g *OpenAIImageGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	return &agency.GeneratorResult{
		TaskID:  taskID,
		Status:  config.STATUS_SUCCESS,
		Outputs: []agency.OutputResource{},
		Raw:     map[string]interface{}{"note": "image generation is synchronous"},
	}, nil
}

// CancelTask is a no-op: there is no task to cancel once Generate returned.
func (g *OpenAIImageGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return nil
}

// collectOutputs reads data[].url and data[].b64_json. Base64 images are
// written under the server's outputs directory so they get served (and later
// published to the tenant bucket) like any other provider URL.
func (g *OpenAIImageGenerator) collectOutputs(taskID string, result map[string]interface{}) ([]agency.OutputResource, error) {
	outputs := []agency.OutputResource{}
	data, _ := result["data"].([]interface{})
	for i, raw := range data {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if url, _ := entry["url"].(string); strings.TrimSpace(url) != "" {
			outputs = append(outputs, agency.OutputResource{URL: strings.TrimSpace(url), Type: "image"})
			continue
		}
		if b64, _ := entry["b64_json"].(string); strings.TrimSpace(b64) != "" {
			res, err := SaveBase64Image(taskID, i, b64)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, res)
		}
	}
	return outputs, nil
}

// request POSTs the payload and decodes the JSON answer.
func (g *OpenAIImageGenerator) request(endpoint string, payload map[string]interface{}, apiKey string) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal image payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create image request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("image request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read image response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("image response parse error: %s", truncateBody(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("image API error %d: %s", resp.StatusCode, agency.ExtractError(result, string(raw)))
	}
	return result, nil
}

// truncateBody bounds an unparseable body so the error stays readable.
func truncateBody(raw []byte) string {
	const max = 400
	if len(raw) <= max {
		return string(raw)
	}
	return string(raw[:max]) + "…"
}
