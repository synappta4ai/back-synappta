// Gemini Nano / Pro generators (Google, synchronous). Wires the
// gemini-nano-banana and gemini-3-pro-image-preview image models into the
// agency pipeline; the Pro constructor lives in gemini_nano_pro.go.
package agencyimage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/utils"
)

// ─── Gemini Nano / Pro (Google, synchronous) ────────────────────

// GeminiNanoGenerator runs Gemini image generation (nano banana / pro).
type GeminiNanoGenerator struct {
	httpClient *http.Client
	label      string
}

// NewGeminiNanoGenerator builds the standard Gemini image generator.
func NewGeminiNanoGenerator() *GeminiNanoGenerator {
	return &GeminiNanoGenerator{httpClient: &http.Client{Timeout: 120 * time.Second}, label: "gemini-nano"}
}

// Name returns the generator name.
func (g *GeminiNanoGenerator) Name() string { return g.label }

// ContentType returns "image".
func (g *GeminiNanoGenerator) ContentType() string { return "image" }

// Match reports whether this generator handles the model name.
func (g *GeminiNanoGenerator) Match(modelName string) bool {
	lower := strings.ToLower(modelName)
	if g.label == "gemini-nano-pro" {
		return strings.Contains(lower, "gemini-3-pro") || strings.Contains(lower, "gemini-nano-pro")
	}
	return strings.Contains(lower, "gemini") && !strings.Contains(lower, "gemini-3-pro")
}

// Validate checks the request against Gemini constraints.
func (g *GeminiNanoGenerator) Validate(req *agency.GeneratorRequest) error {
	errs := agency.ValidateCommon(req)
	if errs.HasErrors() {
		return errs
	}
	if req.Resolution != "" && !ValidResolutionsImage[req.Resolution] {
		errs.Add("resolution", "must be one of: 1K, 2K, 4K")
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

// BuildPayload builds the Gemini generateContent payload.
func (g *GeminiNanoGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
	textPart := agency.CompileContentText(req.Content)

	parts := []map[string]interface{}{{"text": textPart}}
	for _, item := range req.Content {
		if item.Type != "image" || item.DataURL == "" {
			continue
		}
		if imgPart := buildImagePart(item.DataURL); imgPart != nil {
			parts = append(parts, imgPart)
		}
	}

	payload := map[string]interface{}{
		"contents": []map[string]interface{}{{"parts": parts}},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"TEXT", "IMAGE"},
			"responseFormat": map[string]interface{}{
				"image": map[string]interface{}{
					"aspectRatio": mapAspectRatio(req.Ratio),
					"imageSize":   mapImageSize(req.Resolution),
				},
			},
		},
	}
	return payload
}

func mapAspectRatio(ratio string) string {
	switch ratio {
	case "2:3":
		return "ASPECT_RATIO_TWO_BY_THREE"
	case "3:2":
		return "ASPECT_RATIO_THREE_BY_TWO"
	case "3:4":
		return "ASPECT_RATIO_THREE_BY_FOUR"
	case "4:3":
		return "ASPECT_RATIO_FOUR_BY_THREE"
	case "4:5":
		return "ASPECT_RATIO_FOUR_BY_FIVE"
	case "5:4":
		return "ASPECT_RATIO_FIVE_BY_FOUR"
	default:
		return "ASPECT_RATIO_ONE_BY_ONE"
	}
}

func mapImageSize(resolution string) string {
	switch resolution {
	case "2K":
		return "IMAGE_SIZE_TWO_K"
	case "4K":
		return "IMAGE_SIZE_FOUR_K"
	default:
		return "IMAGE_SIZE_ONE_K"
	}
}

// buildImagePart converts a DataURL to a Gemini inline_data part.
func buildImagePart(dataURL string) map[string]interface{} {
	if strings.HasPrefix(dataURL, "data:") {
		commaIdx := strings.Index(dataURL, ",")
		if commaIdx < 0 {
			return nil
		}
		header := dataURL[:commaIdx]
		encoded := dataURL[commaIdx+1:]

		mimeType := "image/png"
		parts := strings.Split(header[5:], ";")
		if len(parts) > 0 && parts[0] != "" {
			mimeType = parts[0]
		}
		return map[string]interface{}{
			"inline_data": map[string]interface{}{"mime_type": mimeType, "data": encoded},
		}
	}

	if strings.HasPrefix(dataURL, "http://") || strings.HasPrefix(dataURL, "https://") {
		ext := strings.ToLower(filepath.Ext(dataURL))
		mimeType := "image/png"
		switch ext {
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
		case ".webp":
			mimeType = "image/webp"
		case ".gif":
			mimeType = "image/gif"
		}
		data, _ := utils.DownloadFromURL(dataURL)
		return map[string]interface{}{
			"inline_data": map[string]interface{}{
				"data":      base64.StdEncoding.EncodeToString(data),
				"mime_type": mimeType,
			},
		}
	}
	return nil
}

// Generate runs the synchronous Gemini image generation.
func (g *GeminiNanoGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
	payload := g.BuildPayload(req)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Gemini URL: endpoint from the model route, else legacy {base}/models/{model}:generateContent
	apiURL := strings.TrimSuffix(req.BaseURL, "/")
	if req.Endpoint != "" {
		apiURL += req.Endpoint
	} else {
		apiURL += "/models/" + req.Model + ":generateContent"
	}
	httpReq, err := http.NewRequest("POST", apiURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("x-goog-api-key", req.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read gemini response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("gemini API %d: %s", resp.StatusCode, string(respBytes))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to parse gemini response: %s", string(respBytes))
	}

	taskID := fmt.Sprintf("gemini_%d", time.Now().UnixMilli())
	var outputs []agency.OutputResource

	if candidates, ok := result["candidates"].([]interface{}); ok && len(candidates) > 0 {
		cand, _ := candidates[0].(map[string]interface{})
		content, _ := cand["content"].(map[string]interface{})
		parts, _ := content["parts"].([]interface{})

		for i, part := range parts {
			p, _ := part.(map[string]interface{})
			inlineData, _ := p["inlineData"].(map[string]interface{})
			if inlineData == nil {
				inlineData, _ = p["inline_data"].(map[string]interface{})
			}
			if inlineData == nil {
				continue
			}
			mimeType, _ := inlineData["mimeType"].(string)
			if mimeType == "" {
				mimeType, _ = inlineData["mime_type"].(string)
			}
			data, _ := inlineData["data"].(string)
			if data == "" {
				continue
			}
			imageBytes, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				continue
			}

			ext := ".png"
			if strings.Contains(mimeType, "jpeg") || strings.Contains(mimeType, "jpg") {
				ext = ".jpg"
			} else if strings.Contains(mimeType, "webp") {
				ext = ".webp"
			}

			outputFilename := fmt.Sprintf("gemini_%s_%d%s", taskID[7:], i, ext)
			outputsDir := filepath.Join(".", config.OutPutUrl())
			outputPath := filepath.Join(outputsDir, outputFilename)
			if err := os.MkdirAll(outputsDir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create outputs dir: %w", err)
			}
			if err := os.WriteFile(outputPath, imageBytes, 0644); err != nil {
				return nil, fmt.Errorf("failed to write image: %w", err)
			}
			outputs = append(outputs, agency.OutputResource{
				URL:  config.OutPutUrl() + "/" + outputFilename,
				Type: "image",
			})
		}
	}

	if len(outputs) == 0 {
		return nil, fmt.Errorf("gemini: no image data found in response")
	}

	return &agency.GeneratorResult{
		TaskID: taskID, Model: req.Model, Status: config.STATUS_SUCCESS,
		Outputs: outputs, Raw: result,
	}, nil
}

// GetStatus is a no-op (synchronous generation).
func (g *GeminiNanoGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
	return &agency.GeneratorResult{
		TaskID: taskID, Status: config.STATUS_SUCCESS, Outputs: []agency.OutputResource{},
	}, nil
}

// CancelTask is a no-op.
func (g *GeminiNanoGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
	return nil
}
