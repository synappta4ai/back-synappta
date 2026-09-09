// Seedance 2.0 generator (BytePlus Ark). Wires the base and pro video
// models into the agency pipeline, including the gallery variant
// (internal/modules/agency/video/seedance_gallery.go).
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

// ModelDreaminaSeedance2 is the real BytePlus model id used in payloads.
const ModelDreaminaSeedance2 = "dreamina-seedance-2-0-260128"

// SeedanceGenerator runs async video generation through BytePlus Ark.
type SeedanceGenerator struct {
	httpClient *http.Client
	gallery    bool // gallery variant (assets referenced as asset://)
	label      string
}

// NewSeedanceGenerator builds the standard Seedance generator.
func NewSeedanceGenerator() *SeedanceGenerator {
	return &SeedanceGenerator{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		label:      "seedance",
	}
}

// Name returns the generator name.
func (g *SeedanceGenerator) Name() string { return g.label }

// ContentType returns "video".
func (g *SeedanceGenerator) ContentType() string { return "video" }

// Match reports whether this generator handles the model name.
func (g *SeedanceGenerator) Match(modelName string) bool {
	lower := strings.ToLower(modelName)
	if g.gallery {
		return strings.Contains(lower, "dreamina-seedance-2-0-gallery")
	}
	return strings.Contains(lower, "dreamina-seedance-2-0") &&
		!strings.Contains(lower, "gallery")
}

// Validate checks the request against Seedance constraints.
func (g *SeedanceGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}

	if req.Duration < 4 || req.Duration > 15 {
		errs.Add("duration", "must be between 4 and 15 seconds")
	}
	if req.Ratio != "" && !ValidRatios[req.Ratio] {
		errs.Add("ratio", "unsupported value: "+req.Ratio)
	}
	if req.Resolution != "" && !ValidResolutionsVideo[req.Resolution] {
		errs.Add("resolution", "must be one of: 480p, 720p, 1080p")
	}
	if req.GenerateAudio && isFastModel(req.Model) {
		errs.Add("generate_audio", "only supported on pro models (non-fast)")
	}
	if errs.HasErrors() {
		return errs
	}
	return nil
}

// Generate submits the async video task.
func (g *SeedanceGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
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
func (g *SeedanceGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	result, err := g.doRequest(baseURL+endpoint+"/"+taskID, "GET", nil, apiKey)
	if err != nil {
		return nil, err
	}

	status, _ := result["status"].(string)

	if status == config.STATUS_SUCCESS {
		videoURL := findVideoURL(result, 0)
		if videoURL != "" {
			localName := fmt.Sprintf("seedance_%d_%s.mp4", time.Now().UnixMilli(), taskID)
			outputs := []agency.OutputResource{{URL: videoURL, Type: "video"}}

			localURL, err := utils.SaveURLOutput(videoURL, localName)
			if err == nil {
				outputs[0].LocalURL = localURL
			}

			return &agency.GeneratorResult{
				TaskID:  taskID,
				Model:   ModelDreaminaSeedance2,
				Status:  status,
				Outputs: outputs,
				Raw:     result,
			}, nil
		}

		return &agency.GeneratorResult{
			TaskID:  taskID,
			Model:   ModelDreaminaSeedance2,
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
			TaskID: taskID, Model: ModelDreaminaSeedance2, Status: status,
			Outputs: []agency.OutputResource{}, Raw: result, Error: errorMsg,
		}, nil
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: ModelDreaminaSeedance2, Status: status,
		Outputs: []agency.OutputResource{}, Raw: result,
	}, nil
}

// CancelTask cancels the async task.
func (g *SeedanceGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	_, err := g.doRequest(baseURL+endpoint+"/"+taskID, "DELETE", nil, apiKey)
	return err
}

// BuildPayload builds the BytePlus Ark content-generation payload.
func (g *SeedanceGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
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

	duration := req.Duration
	if duration <= 0 {
		duration = 5
	}

	payload := map[string]interface{}{
		"model":          ModelDreaminaSeedance2,
		"content":        content,
		"duration":       duration,
		"camerafixed":    req.CameraFixed,
		"watermark":      req.Watermark,
		"generate_audio": req.GenerateAudio,
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
	return payload
}

func (g *SeedanceGenerator) doRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
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
		return nil, fmt.Errorf("seedance: %s", string(respBytes))
	}

	if resp.StatusCode >= 400 {
		msg := agency.ExtractError(result, string(respBytes))
		return nil, fmt.Errorf("seedance %d: %s", resp.StatusCode, msg)
	}
	return result, nil
}
