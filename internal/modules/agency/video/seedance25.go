// Seedance 2.5 generator. Wires the PS2.5 model into the agency pipeline.
//
// Differences vs Seedance 2.0 (internal/modules/agency/video/video.go):
//   - Model id "dreamina-seedance-2-5".
//   - duration: only [4,30] seconds or -1 (auto). No default of 5s:
//     an unset duration is omitted so the API uses its -1 default.
//   - Supports the "adaptive" ratio (mandatory for image-to-video, edit
//     and extend modes).
//   - camera_fixed is NOT supported by the 2.5 API, so it is never sent.
//   - The 2.5 API decides the reference task type (auto/reference/edit/
//     extend) from the submitted content; this generator only labels the
//     reference roles the unified content model can express.
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

// ModelDreaminaSeedance25 is the real BytePlus model id used in payloads.
const ModelDreaminaSeedance25 = "dreamina-seedance-2-5"

// Seedance25Generator runs async video generation through BytePlus Ark.
type Seedance25Generator struct {
	httpClient *http.Client
	label      string
}

// NewSeedance25Generator builds the Seedance 2.5 generator.
func NewSeedance25Generator() *Seedance25Generator {
	return &Seedance25Generator{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		label:      "seedance25",
	}
}

// Name returns the generator name.
func (g *Seedance25Generator) Name() string { return g.label }

// ContentType returns "video".
func (g *Seedance25Generator) ContentType() string { return "video" }

// Match reports whether this generator handles the model name.
func (g *Seedance25Generator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "dreamina-seedance-2-5")
}

// Validate checks the request against Seedance 2.5 constraints.
func (g *Seedance25Generator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}

	if req.Duration != 0 && (req.Duration < 4 || req.Duration > 30) {
		errs.Add("duration", "must be between 4 and 30 seconds, or 0 for auto")
	}
	if req.Ratio != "" && req.Ratio != "adaptive" && !ValidRatios[req.Ratio] {
		errs.Add("ratio", "unsupported value: "+req.Ratio)
	}
	if req.Resolution != "" && !ValidResolutionsVideo[req.Resolution] {
		errs.Add("resolution", "must be one of: 480p, 720p, 1080p")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// Generate submits the async video task.
func (g *Seedance25Generator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)

	result, err := g.doRequest(req.BaseURL+req.Endpoint, "POST", payload, req.APIKey)
	if err != nil {
		return nil, err
	}

	taskID, _ := result["id"].(string)
	if taskID == "" {
		taskID, _ = result["task_id"].(string)
	}
	if taskID == "" {
		return nil, fmt.Errorf("no task ID in response")
	}

	return &agency.GeneratorResult{
		TaskID:  taskID,
		Model:   req.Model,
		Status:  config.STATUS_RUNNING,
		Outputs: []agency.OutputResource{},
		Raw:     result,
	}, nil
}

// GetStatus polls the async task and downloads the video on success.
func (g *Seedance25Generator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	result, err := g.doRequest(baseURL+endpoint+"/"+taskID, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}

	status, _ := result["status"].(string)

	if status == config.STATUS_SUCCESS {
		videoURL := findVideoURL(result, 0)
		if videoURL != "" {
			localName := fmt.Sprintf("seedance25_%d_%s.mp4", time.Now().UnixMilli(), taskID)
			outputs := []agency.OutputResource{{URL: videoURL, Type: "video"}}

			localURL, err := utils.SaveURLOutput(videoURL, localName)
			if err == nil {
				outputs[0].LocalURL = localURL
			}

			return &agency.GeneratorResult{
				TaskID:  taskID,
				Model:   ModelDreaminaSeedance25,
				Status:  status,
				Outputs: outputs,
				Raw:     result,
			}, nil
		}

		return &agency.GeneratorResult{
			TaskID:  taskID,
			Model:   ModelDreaminaSeedance25,
			Status:  "succeeded_no_url",
			Outputs: []agency.OutputResource{},
			Raw:     result,
			Error:   "Job succeeded but no video URL was found in the response.",
		}, nil
	}

	if status == config.STATUS_FAILED {
		errorMsg, _ := result["error"].(string)
		if errorMsg == "" {
			if e, ok := result["error"].(map[string]interface{}); ok {
				errorMsg, _ = e["message"].(string)
			}
		}
		return &agency.GeneratorResult{
			TaskID: taskID, Model: ModelDreaminaSeedance25, Status: status,
			Outputs: []agency.OutputResource{}, Raw: result, Error: errorMsg,
		}, nil
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: ModelDreaminaSeedance25, Status: status,
		Outputs: []agency.OutputResource{}, Raw: result,
	}, nil
}

// CancelTask cancels the async task.
func (g *Seedance25Generator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	_, err := g.doRequest(baseURL+endpoint+"/"+taskID, "DELETE", nil, apiKey)
	return err
}

// BuildPayload builds the BytePlus Ark content-generation payload.
func (g *Seedance25Generator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	content := make([]map[string]interface{}, 0)

	textPart := agency.CompileContentText(req.Content)
	content = append(content, map[string]interface{}{
		"type": "text",
		"text": textPart,
	})

	for _, item := range req.Content {
		switch item.Type {
		case "image":
			if item.DataURL == "" {
				continue
			}
			content = append(content, map[string]interface{}{
				"type":      "image_url",
				"image_url": map[string]string{"url": item.DataURL},
				"role":      "reference_image",
			})
		case "video":
			if item.DataURL == "" {
				continue
			}
			content = append(content, map[string]interface{}{
				"type":      "video_url",
				"video_url": map[string]string{"url": item.DataURL},
				"role":      "reference_video",
			})
		case "audio":
			if item.DataURL == "" {
				continue
			}
			content = append(content, map[string]interface{}{
				"type":      "audio_url",
				"audio_url": map[string]string{"url": item.DataURL},
				"role":      "reference_audio",
			})
		}
	}

	payload := map[string]interface{}{
		"model":   ModelDreaminaSeedance25,
		"content": content,
	}
	// Duration: only [4,30] or auto (-1). Omit when unset so the API
	// applies its -1 default (required for edit mode).
	if req.Duration > 0 {
		payload["duration"] = req.Duration
	}
	if req.Ratio != "" {
		payload["ratio"] = req.Ratio
	}
	if req.Resolution != "" {
		payload["resolution"] = req.Resolution
	}
	if !req.GenerateAudio {
		payload["generate_audio"] = false
	}
	if req.Watermark {
		payload["watermark"] = true
	}
	return payload
}

// doRequest sends a request to the BytePlus Ark API.
func (g *Seedance25Generator) doRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
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
		return nil, fmt.Errorf("seedance25: %s", string(respBytes))
	}

	if resp.StatusCode >= 400 {
		msg := agency.ExtractError(result, string(respBytes))
		return nil, fmt.Errorf("seedance25 %d: %s", resp.StatusCode, msg)
	}
	return result, nil
}
