// Package agency is the unified AI generation orchestration core (the
// Synapta equivalent of dcs-back's "studio"). It dispatches generation
// requests to modality generators (video, audio, image, text), records
// generation logs, server communications and generated assets, and manages
// the BytePlus gallery asset sync.
//
// One Core instance exists per tenant (stores are bound to the tenant's
// connection pool and credentials).
package agency

import (
	"fmt"
	"strings"
)

// ─── Unified payload types ──────────────────────────────────────

// ContentItem represents a single entry in the content array.
// When received from the client, only Type/Text/Name/ID are populated.
// DataURL is set by resolveContent() before passing to the generator pipeline.
type ContentItem struct {
	Type    string `json:"type" binding:"required"` // "text", "image", "video", "audio"
	Text    string `json:"text,omitempty"`          // prompt text or asset description
	Name    string `json:"name,omitempty"`          // original filename (file types)
	ID      string `json:"id,omitempty"`            // file UUID from the file store
	DataURL string `json:"-"`                       // resolved data URL (populated by service)
}

// GenerateRequest is the unified payload for POST /agency/*/generate.
// Event context fields are mandatory: every generation must hang off a piece.
type GenerateRequest struct {
	Model         string        `json:"model" binding:"required"`
	Content       []ContentItem `json:"content" binding:"required"`
	Ratio         string        `json:"ratio"`
	Duration      float64       `json:"duration"`
	CameraFixed   *bool         `json:"camerafixed"`
	Seed          string        `json:"seed"`
	Quality       string        `json:"quality"`
	Quantity      int           `json:"quantity"`
	Watermark     *bool         `json:"watermark"`
	Resolution    string        `json:"resolution"`
	GenerateAudio *bool         `json:"generate_audio"`
	ImageMode     string        `json:"image_mode"`

	// Event tracking — obligatorio para registrar en logs y recuperar estado.
	EventName        string `json:"event_name"`
	UserName         string `json:"user_name"`
	EventID          string `json:"event_id" binding:"required"`
	ProgramID        string `json:"program_id"`
	PieceID          string `json:"piece_id" binding:"required"`
	PieceCode        string `json:"piece_code" binding:"required"`
	GenerationNumber int    `json:"generation_number" binding:"required"`
	UserID           int    `json:"user_id"`
	// ResourceType — lo setea cada dominio (video/image/audio/text) automáticamente.
	ResourceType string `json:"-"`
}

// OutputResource represents a single generated output (video, image, audio, text).
type OutputResource struct {
	URL      string `json:"url"`
	LocalURL string `json:"localUrl,omitempty"`
	Type     string `json:"type"` // "video", "image", "audio", "text"
}

// GenerateResponse is returned by POST /agency/*/generate.
type GenerateResponse struct {
	TaskID  string           `json:"taskId"`
	Model   string           `json:"model"`
	Status  string           `json:"status"`
	Outputs []OutputResource `json:"outputs,omitempty"`
}

// StatusResponse is returned by GET /agency/*/status/:taskId.
type StatusResponse struct {
	Status   string           `json:"status"`
	Outputs  []OutputResource `json:"outputs,omitempty"`
	Error    string           `json:"error,omitempty"`
	Raw      interface{}      `json:"raw,omitempty"`
	Progress interface{}      `json:"progress,omitempty"`
	// Estimated progress percent (0-100) computed server-side while the task
	// runs; 100 once it reaches a terminal state. Never decreases.
	ProgressPercent int `json:"progress_percent"`
}

// StatusResult is the internal status shape.
type StatusResult struct {
	Status   string      `json:"status"`
	VideoURL string      `json:"videoUrl,omitempty"`
	LocalURL string      `json:"localUrl,omitempty"`
	ImageURL string      `json:"imageUrl,omitempty"`
	Raw      interface{} `json:"raw,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// PreviewPayloadResponse returns the AI API payload without sending it.
type PreviewPayloadResponse struct {
	Model       string                 `json:"model"`
	Endpoint    string                 `json:"endpoint"`
	Payload     map[string]interface{} `json:"payload"`
	ContentType string                 `json:"content_type"`
}

// VideoMetadata carries the important provider-side facts about a generated
// video, persisted into generation_logs and returned in the status response.
type VideoMetadata struct {
	UsageTokens           int64
	UsageCompletionTokens int64
	Duration              int
	Resolution            string
	Ratio                 string
	Seed                  int64
	FPS                   int
	Progress              int
}

// ─── Generator pipeline types (shared across all domain generators) ─

// GeneratorRequest is the unified request payload for the generator pipeline.
type GeneratorRequest struct {
	Model         string
	Content       []ContentItem
	Ratio         string
	Duration      int
	CameraFixed   bool
	Seed          string
	Quality       string
	Quantity      int
	Watermark     bool
	Resolution    string
	GenerateAudio bool
	InputDuration float64
	ImageMode     string
	APIKey        string
	BaseURL       string
	Endpoint      string
}

// GeneratorResult is the response returned by a generator.
type GeneratorResult struct {
	TaskID  string           `json:"taskId"`
	Model   string           `json:"model"`
	Status  string           `json:"status"`
	Outputs []OutputResource `json:"outputs,omitempty"`
	Raw     interface{}      `json:"raw,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// PipelineRunner is the internal interface satisfied by all domain generators
// (video.Generator, image.Generator, etc.) for the unified pipeline.
type PipelineRunner interface {
	Match(modelName string) bool
	Validate(req *GeneratorRequest) error
	Generate(req *GeneratorRequest) (*GeneratorResult, error)
	GetStatus(taskID, apiKey, baseURL, endpoint string) (*GeneratorResult, error)
	CancelTask(taskID, apiKey, baseURL, endpoint string) error
	BuildPayload(req *GeneratorRequest) map[string]interface{}
	ContentType() string
	Name() string
}

// CostCalculator defines how to compute the estimated cost of a generation.
type CostCalculator interface {
	// Match returns true if this calculator handles the given model name.
	Match(modelName string) bool
	// CalculateFromResponse extracts cost directly from an API response.
	// Returns (cost, true) if the API included cost info, (0, false) otherwise.
	CalculateFromResponse(raw interface{}, req *GeneratorRequest) (float64, bool)
	// CalculateEstimated computes cost from request parameters (fallback).
	CalculateEstimated(req *GeneratorRequest) float64
	// NeedsBackgroundCalc returns true if CalculateEstimated should run in background.
	NeedsBackgroundCalc() bool
	// Name returns a human-readable name for this calculator.
	Name() string
}

// PushNotifier is satisfied by the push module's service. Wired via
// SetPushNotifier so the core can alert a user when an async task completes.
type PushNotifier interface {
	SendToUser(userID int64, title, body string, data map[string]string)
}

// GenerationSaver persists completed generation outputs to the piece's
// generation row (video_url / video_local_url). taskID links the generation
// with its generation_log for payload retrieval.
type GenerationSaver func(pieceID string, generationNumber int, videoURL, videoLocalURL, taskID string) error

// ─── Common validation ──────────────────────────────────────────

// ValidationError accumulates multiple validation errors.
type ValidationError struct {
	Fields []string
}

func (e *ValidationError) Error() string {
	return "validation failed: " + strings.Join(e.Fields, "; ")
}

func (e *ValidationError) Add(field, msg string) {
	e.Fields = append(e.Fields, field+": "+msg)
}

func (e *ValidationError) HasErrors() bool {
	return len(e.Fields) > 0
}

// ValidateCommon checks fields shared across all generators.
func ValidateCommon(req *GeneratorRequest) *ValidationError {
	errs := &ValidationError{}

	if strings.TrimSpace(req.Model) == "" {
		errs.Add("model", "is required")
	}
	if len(req.Content) == 0 {
		errs.Add("content", "must have at least one item")
	} else {
		hasText := false
		for i, item := range req.Content {
			if item.Type == "" {
				errs.Add(fmt.Sprintf("content[%d]", i), "type is required (text, image, video, audio)")
			}
			if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
				hasText = true
			}
		}
		if !hasText {
			errs.Add("content", "must include at least one text item with a prompt")
		}
	}

	return errs
}

// CompileContentText concatenates text-type items into a single prompt string.
func CompileContentText(items []ContentItem) string {
	var parts []string
	for _, item := range items {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Text))
		}
	}
	textBlock := strings.Join(parts, ". ")
	if textBlock != "" && !strings.HasSuffix(textBlock, ".") {
		textBlock += "."
	}
	return textBlock
}

// ExtractError extracts a human-readable error message from an API response.
func ExtractError(result map[string]interface{}, raw string) string {
	if e, ok := result["error"].(map[string]interface{}); ok {
		if msg, ok := e["message"].(string); ok {
			return msg
		}
	}
	if msg, ok := result["message"].(string); ok {
		return msg
	}
	if len(raw) > 400 {
		raw = raw[:400]
	}
	return raw
}
