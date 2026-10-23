// Gemini Nano / Pro generators (Google, synchronous). Wires the
// gemini-nano-banana and gemini-3-pro-image-preview image models into the
// agency pipeline; the Pro constructor lives in gemini_nano_pro.go.
package agencyimage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/utils"
)

// Gemini rejects requests whose input exceeds ~8 MB. Base64 inflates the raw
// image by ~33%, so reference photos straight from the library (up to 10 MB
// each) blow the limit on their own. Keep a margin for the prompt and the JSON
// envelope and compact every inline image that would not fit.
const (
	geminiInlineBudget  = 7 << 20 // base64 bytes for all inline images
	geminiInlineMaxSide = 2048    // pixels on the longest side
	geminiInlineQuality = 85
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
//
// Hosted inference models are namespaced "<provider>/<model id>" (e.g.
// "openrouter/google/gemini-3-pro-image-preview") and belong to the
// OpenAI-compatible generator. Claiming them here sent an oversized Gemini
// payload to the provider's /images route, which answered with the misleading
// "total text input size exceeds 8 MB" gateway error.
func (g *GeminiNanoGenerator) Match(modelName string) bool {
	name := strings.TrimSpace(modelName)
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	lower := strings.ToLower(name)
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

	images := make([]string, 0, len(req.Content))
	for _, item := range req.Content {
		if item.Type == "image" && item.DataURL != "" {
			images = append(images, item.DataURL)
		}
	}
	// Split the base64 budget evenly: every image is compacted to its share,
	// so the whole request stays under Gemini's input cap regardless of how
	// many references the user attached.
	perImage := geminiInlineBudget
	if len(images) > 1 {
		perImage = geminiInlineBudget / len(images)
	}

	parts := []map[string]interface{}{{"text": textPart}}
	for _, dataURL := range images {
		if imgPart := buildImagePart(dataURL, perImage); imgPart != nil {
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

// buildImagePart converts a reference (data URL or public serve URL) to a
// Gemini inline_data part, compacting it to maxEncoded base64 bytes.
func buildImagePart(dataURL string, maxEncoded int) map[string]interface{} {
	raw, mimeType, err := readImageBytes(dataURL)
	if err != nil {
		log.Printf("[gemini] reference image skipped: %v", err)
		return nil
	}
	if maxEncoded <= 0 {
		maxEncoded = geminiInlineBudget
	}
	raw, mimeType = compactImage(raw, mimeType, maxEncoded)
	return map[string]interface{}{
		"inline_data": map[string]interface{}{
			"mime_type": mimeType,
			"data":      base64.StdEncoding.EncodeToString(raw),
		},
	}
}

// readImageBytes returns the image bytes for a data URL or an http(s) URL and
// sniffs its real MIME type, so a mislabeled upload is not forwarded as-is.
func readImageBytes(dataURL string) ([]byte, string, error) {
	if strings.HasPrefix(dataURL, "data:") {
		commaIdx := strings.Index(dataURL, ",")
		if commaIdx < 0 {
			return nil, "", fmt.Errorf("malformed data URL")
		}
		header := dataURL[5:commaIdx]
		mimeType := "image/png"
		if parts := strings.Split(header, ";"); len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			mimeType = strings.TrimSpace(parts[0])
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(dataURL[commaIdx+1:]))
		if err != nil {
			return nil, "", fmt.Errorf("decode data URL: %w", err)
		}
		if detected := http.DetectContentType(raw); strings.HasPrefix(detected, "image/") {
			mimeType = detected
		}
		return raw, mimeType, nil
	}

	if strings.HasPrefix(dataURL, "http://") || strings.HasPrefix(dataURL, "https://") {
		raw, err := utils.DownloadFromURL(dataURL)
		if err != nil {
			return nil, "", fmt.Errorf("download reference: %w", err)
		}
		mimeType := http.DetectContentType(raw)
		if !strings.HasPrefix(mimeType, "image/") {
			mimeType = "image/png"
		}
		return raw, mimeType, nil
	}

	return nil, "", fmt.Errorf("unsupported reference %q", dataURL)
}

// compactImage re-encodes an oversized image as JPEG, shrinking it until its
// base64 representation fits maxEncoded. Small images are returned untouched.
func compactImage(raw []byte, mimeType string, maxEncoded int) ([]byte, string) {
	if base64.StdEncoding.EncodedLen(len(raw)) <= maxEncoded {
		return raw, mimeType
	}

	src, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return raw, mimeType
	}

	for _, maxSide := range []int{geminiInlineMaxSide, 1600, 1280, 1024, 800} {
		fitted := fitWithin(src, maxSide)
		for _, quality := range []int{geminiInlineQuality, 75, 65} {
			var buf bytes.Buffer
			if err := imaging.Encode(&buf, fitted, imaging.JPEG, imaging.JPEGQuality(quality)); err != nil {
				return raw, mimeType
			}
			if base64.StdEncoding.EncodedLen(buf.Len()) <= maxEncoded {
				return buf.Bytes(), "image/jpeg"
			}
		}
	}

	// Last resort: the smallest encoding we can produce. If it still exceeds
	// the per-image share, Gemini reports the limit and the user can drop a
	// reference instead of receiving a silently truncated request.
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, fitWithin(src, 512), imaging.JPEG, imaging.JPEGQuality(60)); err == nil {
		return buf.Bytes(), "image/jpeg"
	}
	return raw, mimeType
}

// fitWithin scales an image so its longest side is at most maxSide.
func fitWithin(src image.Image, maxSide int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= maxSide && height <= maxSide {
		return src
	}
	if width >= height {
		return imaging.Resize(src, maxSide, 0, imaging.Lanczos)
	}
	return imaging.Resize(src, 0, maxSide, imaging.Lanczos)
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
