// RunPod serverless generator.
//
// RunPod exposes one queue-based endpoint per model:
//
//	POST /v2/{endpoint_id}/run            → {id, status:"IN_QUEUE"}
//	GET  /v2/{endpoint_id}/status/{job}   → {status, output}
//	POST /v2/{endpoint_id}/cancel/{job}
//
// The model name is "runpod/<endpoint-id>" and the core resolves the route as
// "/v2/<endpoint-id>", so the generator only appends /run, /status or /cancel.
//
// RunPod does NOT define the input/output schema: every worker decides it.
// This generator speaks the common text-to-image dialect (prompt, width,
// height, seed, num_outputs) and accepts URLs or base64 under the usual image
// fields. Workers that only accept a ComfyUI `workflow` need their own adapter.
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

// runPodStatuses maps RunPod job states onto Synapta's.
var runPodStatuses = map[string]string{
	"IN_QUEUE":    config.STATUS_RUNNING,
	"IN_PROGRESS": config.STATUS_RUNNING,
	"COMPLETED":   config.STATUS_SUCCESS,
	"FAILED":      config.STATUS_FAILED,
	"CANCELLED":   config.STATUS_CANCELLED,
	"TIMED_OUT":   config.STATUS_FAILED,
}

// runPodTimeout bounds each RunPod call. Submit is asynchronous (POST /run),
// so this only covers one HTTP round trip, not the whole generation.
const runPodTimeout = 60 * time.Second

// RunPodGenerator runs image generation on a RunPod serverless endpoint.
type RunPodGenerator struct {
	httpClient *http.Client
}

// NewRunPodGenerator builds the RunPod generator.
func NewRunPodGenerator() *RunPodGenerator {
	return &RunPodGenerator{httpClient: &http.Client{Timeout: runPodTimeout}}
}

// Name returns the generator name declared by discovered entries.
func (g *RunPodGenerator) Name() string { return model.GeneratorRunPod }

// ContentType returns "image".
func (g *RunPodGenerator) ContentType() string { return "image" }

// Match reports whether the model is a RunPod serverless endpoint.
func (g *RunPodGenerator) Match(modelName string) bool {
	provider, _, ok := model.ParseInferenceModel(modelName)
	return ok && model.IsEndpointScopedProvider(provider)
}

// Validate checks the request against the RunPod image dialect.
func (g *RunPodGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if _, _, ok := model.ParseInferenceModel(req.Model); !ok {
		errs.Add("model", "is not a RunPod endpoint (expected runpod/<endpoint-id>)")
	}
	if !strings.HasPrefix(req.Endpoint, "/v2/") || strings.TrimPrefix(req.Endpoint, "/v2/") == "" {
		errs.Add("model", "has no RunPod endpoint id — save it with the credential in admin → modelos")
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
	for _, item := range req.Content {
		if item.Type == "image" && item.DataURL != "" {
			errs.Add("content", "reference images are not supported by the RunPod image dialect yet")
			break
		}
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// BuildPayload builds the {"input": {…}} envelope the queue API expects.
func (g *RunPodGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	width, height := ImageDimensionsForRatio(req.Ratio)
	input := map[string]interface{}{
		"prompt": agency.CompileContentText(req.Content),
		"width":  width,
		"height": height,
	}
	if seed, err := strconv.Atoi(strings.TrimSpace(req.Seed)); err == nil {
		input["seed"] = seed
	}
	if req.Quantity > 1 {
		input["num_outputs"] = req.Quantity
	}
	return map[string]interface{}{"input": input}
}

// Generate submits the job asynchronously and returns the RunPod job id.
func (g *RunPodGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)
	endpoint := strings.TrimRight(req.BaseURL, "/") + req.Endpoint + "/run"

	result, err := g.request(http.MethodPost, endpoint, payload, req.AuthKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["id"].(string)
	if taskID == "" {
		return nil, fmt.Errorf("runpod returned no job id: %s", summarize(result))
	}

	status := runPodStatus(result["status"])
	genResult := &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  status,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}
	// /run normally answers IN_QUEUE, but a warm worker can complete before
	// the response is written; collect the images in that case.
	if status == config.STATUS_SUCCESS {
		outputs, oerr := g.outputsFrom(result, taskID, req.Quantity)
		if oerr != nil {
			return nil, oerr
		}
		genResult.Outputs = outputs
	}
	return genResult, nil
}

// GetStatus polls the job and collects images once it completes.
func (g *RunPodGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	url := strings.TrimRight(baseURL, "/") + strings.TrimRight(endpoint, "/") + "/status/" + taskID
	result, err := g.request(http.MethodGet, url, nil, apiKey)
	if err != nil {
		return nil, err
	}

	status := runPodStatus(result["status"])
	genResult := &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   string(model.ProviderRunPod),
		Status:  status,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}
	switch status {
	case config.STATUS_SUCCESS:
		outputs, oerr := g.outputsFrom(result, taskID, 0)
		if oerr != nil {
			return nil, oerr
		}
		if len(outputs) == 0 {
			// Completed without a usable image is a failure: a custom status
			// would poll forever and the log would never close.
			genResult.Status = config.STATUS_FAILED
			genResult.Error = "RunPod completed the job without an image in output."
			return genResult, nil
		}
		genResult.Outputs = outputs
	case config.STATUS_FAILED, config.STATUS_CANCELLED:
		genResult.Error = runPodError(result)
	}
	return genResult, nil
}

// CancelTask cancels a queued or running job.
func (g *RunPodGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	url := strings.TrimRight(baseURL, "/") + strings.TrimRight(endpoint, "/") + "/cancel/" + taskID
	_, err := g.request(http.MethodPost, url, nil, apiKey)
	return err
}

// outputsFrom collects image URLs and base64 payloads from the job result.
func (g *RunPodGenerator) outputsFrom(result map[string]interface{}, taskID string, wanted int) ([]agency.OutputResource, error) {
	candidates := collectRunPodImageStrings(result["output"], nil)
	outputs := make([]agency.OutputResource, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
			outputs = append(outputs, agency.OutputResource{URL: candidate, Type: "image"})
		} else if LooksLikeBase64Image(candidate) {
			res, err := SaveBase64Image(taskID, len(outputs), candidate)
			if err != nil {
				return nil, err
			}
			outputs = append(outputs, res)
		}
		if wanted > 0 && len(outputs) >= wanted {
			break
		}
	}
	return outputs, nil
}

// collectRunPodImageStrings walks the worker output and keeps the strings
// under image-shaped keys (or that decode to an image), so prompts and other
// text fields are never mistaken for results.
func collectRunPodImageStrings(value interface{}, out []string) []string {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "data:image/") || strings.HasPrefix(typed, "http://") ||
			strings.HasPrefix(typed, "https://") || LooksLikeBase64Image(typed) {
			return append(out, typed)
		}
	case []interface{}:
		for _, item := range typed {
			out = collectRunPodImageStrings(item, out)
		}
	case map[string]interface{}:
		for _, item := range typed {
			out = collectRunPodImageStrings(item, out)
		}
	}
	return out
}

// runPodStatus normalizes a RunPod state, defaulting an unknown state to
// running (better a poll too many than a task marked done without output).
func runPodStatus(raw interface{}) string {
	s, _ := raw.(string)
	if mapped, ok := runPodStatuses[strings.ToUpper(strings.TrimSpace(s))]; ok {
		return mapped
	}
	return config.STATUS_RUNNING
}

// runPodError extracts the worker error from a failed job.
func runPodError(result map[string]interface{}) string {
	if msg := agency.ExtractError(result, ""); msg != "" {
		return msg
	}
	return summarize(result)
}

// request sends a RunPod call with Bearer auth and decodes the JSON answer.
func (g *RunPodGenerator) request(method, url string, body interface{}, apiKey string) (map[string]interface{}, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal runpod payload: %w", err)
		}
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to create runpod request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runpod request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read runpod response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("runpod response parse error: %s", truncateBody(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("runpod %d: %s", resp.StatusCode, agency.ExtractError(result, string(raw)))
	}
	return result, nil
}

// summarize renders a small JSON excerpt for error messages.
func summarize(result map[string]interface{}) string {
	raw, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	return truncateBody(raw)
}
