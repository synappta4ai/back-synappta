// Higgsfield generator. Wires Higgsfield video models (Kling 3.0 Turbo
// text-to-video, Wan 2.6 reference-to-video) into the agency pipeline.
//
// API shape (docs.higgsfield.ai):
//   - Auth: "Authorization: Key <API_KEY_ID>:<API_KEY_SECRET>" (NOT Bearer).
//   - Submit: POST {model endpoint} → {"status":"queued","request_id":...,
//     "status_url":"/requests/<id>/status","cancel_url":"/requests/<id>/cancel"}.
//   - Poll: GET /requests/<id>/status → {"status":"queued|running|completed|
//     failed", ...,"video":{"url":...}} on success.
//   - Cancel: POST /requests/<id>/cancel.
//   - Input fields are flat: prompt, duration (int seconds), resolution,
//     aspect_ratio, image_urls / video_urls (public URLs).
package agencyvideo

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
	"synapta/internal/utils"
)

// HiggsfieldBaseURL is the provider API origin (catalog matches it).
const HiggsfieldBaseURL = "https://api.higgsfield.ai"

// higgsfieldStatuses maps Higgsfield task states to Synapta statuses.
var higgsfieldStatuses = map[string]string{
	"queued":    config.STATUS_RUNNING,
	"running":   config.STATUS_RUNNING,
	"in_queue":  config.STATUS_RUNNING,
	"completed": config.STATUS_SUCCESS,
	"failed":    config.STATUS_FAILED,
	"canceled":  config.STATUS_FAILED,
	"cancelled": config.STATUS_FAILED,
}

// HiggsfieldGenerator runs async video generation through Higgsfield.
type HiggsfieldGenerator struct {
	httpClient *http.Client
	label      string
}

// NewHiggsfieldGenerator builds the Higgsfield generator.
func NewHiggsfieldGenerator() *HiggsfieldGenerator {
	return &HiggsfieldGenerator{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		label:      "higgsfield",
	}
}

// Name returns the generator name.
func (g *HiggsfieldGenerator) Name() string { return g.label }

// ContentType returns "video".
func (g *HiggsfieldGenerator) ContentType() string { return "video" }

// Match reports whether this generator handles the model name.
func (g *HiggsfieldGenerator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "higgsfield-")
}

// Validate checks the request against Higgsfield constraints.
func (g *HiggsfieldGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	// Duration/resolution/ratio bounds are model-specific (Kling 3-15s,
	// Wan reference 5/10s); the catalog defaults drive the UI and the API
	// rejects out-of-range values with a clear error, so only reject
	// clearly-invalid values here.
	if req.Duration < 0 {
		errs.Add("duration", "must be a positive number of seconds")
	}
	if req.Resolution != "" && !ValidResolutionsVideo[req.Resolution] {
		errs.Add("resolution", "must be one of: 480p, 720p, 1080p")
	}
	if req.Ratio != "" && !ValidRatios[req.Ratio] {
		errs.Add("ratio", "unsupported value: "+req.Ratio)
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// Generate submits the async video task.
func (g *HiggsfieldGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	authKey := req.AuthKey
	result, err := g.doRequest(req.BaseURL+req.Endpoint, "POST", payload, authKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["request_id"].(string)
	if taskID == "" {
		taskID, _ = result["id"].(string)
	}
	if taskID == "" {
		taskID, _ = result["task_id"].(string)
	}
	if taskID == "" {
		return nil, fmt.Errorf("no request_id in response")
	}

	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  config.STATUS_RUNNING,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}, nil
}

// GetStatus polls the async request and downloads the video on success.
// The core passes creds.AuthKey() as apiKey (Higgsfield needs id:secret).
func (g *HiggsfieldGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	// Shared request-status route: the per-model endpoint is only for submit.
	statusURL := strings.TrimSuffix(baseURL, "/") + "/requests/" + taskID + "/status"
	result, err := g.doRequest(statusURL, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}

	rawStatus, _ := result["status"].(string)
	status := higgsfieldStatus(rawStatus)

	if status == config.STATUS_SUCCESS {
		videoURL := findVideoURL(result, 0)
		if videoURL != "" {
			localName := fmt.Sprintf("higgsfield_%d_%s.mp4", time.Now().UnixMilli(), taskID)
			outputs := []agency.OutputResource{{URL: videoURL, Type: "video"}}

			localURL, err := utils.SaveURLOutput(videoURL, localName)
			if err == nil {
				outputs[0].LocalURL = localURL
			}

			return &agency.GeneratorResult{
				TaskID:  taskID,
				Model:   "higgsfield",
				Status:  status,
				Outputs: outputs,
				Raw:     result,
			}, nil
		}

		return &agency.GeneratorResult{
			TaskID:  taskID,
			Model:   "higgsfield",
			Status:  "succeeded_no_url",
			Outputs: []agency.OutputResource{},
			Raw:     result,
			Error:   "Job completed but no video URL was found in the response.",
		}, nil
	}

	if status == config.STATUS_FAILED {
		errorMsg := higgsfieldError(result)
		return &agency.GeneratorResult{
			TaskID: taskID, Model: "higgsfield", Status: status,
			Outputs: []agency.OutputResource{}, Raw: result, Error: errorMsg,
		}, nil
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: "higgsfield", Status: status,
		Outputs: []agency.OutputResource{}, Raw: result,
	}, nil
}

// CancelTask cancels the async request via its cancel route.
func (g *HiggsfieldGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	cancelURL := strings.TrimSuffix(baseURL, "/") + "/requests/" + taskID + "/cancel"
	_, err := g.doRequest(cancelURL, "POST", nil, apiKey)
	return err
}

// BuildPayload builds the flat Higgsfield JSON body for the endpoint.
// Text-to-video endpoints take prompt/duration/resolution/aspect_ratio.
// Reference endpoints (seedance …/reference-to-video, wan reference-to-video,
// genjutsu…) take image_urls / video_urls / audio_urls arrays; image-to-video
// endpoints (seedance …/image-to-video, model names with "-i2v") take the
// singular image_url. The schemas are strict (additionalProperties:false) and
// providers silently DROP unknown fields, so the request is routed by
// content: one image rides the verified i2v twin; multiple references ride
// the multi-reference route.
func (g *HiggsfieldGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	payload := map[string]interface{}{
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

	imageURLs := make([]string, 0, 3)
	videoURLs := make([]string, 0, 3)
	audioURLs := make([]string, 0, 3)
	for _, item := range req.Content {
		switch item.Type {
		case "image":
			if item.DataURL != "" {
				imageURLs = append(imageURLs, item.DataURL)
			}
		case "video":
			if item.DataURL != "" {
				videoURLs = append(videoURLs, item.DataURL)
			}
		case "audio":
			if item.DataURL != "" {
				audioURLs = append(audioURLs, item.DataURL)
			}
		}
	}

	// Content-based routing: a text-to-video model with reference images gets
	// redirected to its image-to-video twin (when the catalog defines one).
	// Rationale: t2v schemas reject unknown fields, so an image sent as
	// image_urls/image_url to a t2v endpoint is silently ignored and the model
	// "invents" the subject from the prompt alone.
	lowerModel := strings.ToLower(req.Model)
	useSingularImage := false
	switch {
	case strings.Contains(lowerModel, "-i2v"), strings.Contains(lowerModel, "image-to-video"),
		strings.HasSuffix(req.Endpoint, "/image-to-video"):
		// Dedicated image-to-video route: the schema takes one image_url.
		useSingularImage = true
	case strings.Contains(lowerModel, "reference"), strings.HasSuffix(req.Endpoint, "/reference-to-video"):
		// Dedicated multi-reference route: image_urls / video_urls arrays are
		// the documented shape — no rerouting needed.
	default:
		// Content-based routing for text-to-video models. Rationale: t2v
		// schemas reject unknown fields, so references sent to a t2v endpoint
		// are silently ignored and the model "invents" the subject. Exactly
		// one image rides the verified i2v twin; multiple references (or any
		// video/audio) need the multi-reference route, which keeps aspect_ratio.
		switch {
		case len(imageURLs) == 1 && len(videoURLs) == 0 && len(audioURLs) == 0 && req.ImageEndpoint != "":
			req.Endpoint = req.ImageEndpoint
			useSingularImage = true
		case (len(imageURLs) > 1 || len(videoURLs) > 0 || len(audioURLs) > 0) && req.ReferenceEndpoint != "":
			req.Endpoint = req.ReferenceEndpoint
		}
	}

	if len(imageURLs) > 0 {
		if useSingularImage {
			payload["image_url"] = imageURLs[0]
			// The i2v schema has no aspect_ratio: framing follows the input
			// image, so the text-to-video aspect selection must not leak in.
			delete(payload, "aspect_ratio")
		} else {
			payload["image_urls"] = imageURLs
		}
	}
	if len(videoURLs) > 0 {
		payload["video_urls"] = videoURLs
	}
	if len(audioURLs) > 0 {
		payload["audio_urls"] = audioURLs
	}
	return payload
}

// higgsfieldStatus normalizes a Higgsfield status string to Synapta's.
func higgsfieldStatus(raw string) string {
	if mapped, ok := higgsfieldStatuses[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return mapped
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// higgsfieldError extracts a human-readable error from a failed response.
func higgsfieldError(result map[string]interface{}) string {
	for _, key := range []string{"error", "message", "detail"} {
		switch v := result[key].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]interface{}:
			if m, ok := v["message"].(string); ok && m != "" {
				return m
			}
		}
	}
	return ""
}

// doRequest sends a request to the Higgsfield API with Key auth.
func (g *HiggsfieldGenerator) doRequest(url, method string, body interface{}, authKey string) (map[string]interface{}, error) {
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
