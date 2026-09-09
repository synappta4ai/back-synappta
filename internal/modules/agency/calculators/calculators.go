// Package calculators provides CostCalculator implementations for each model family.
// Core calculators (Seedance, Seedance Gallery, Seedream) are consolidated here;
// the Seedance 2.5 calculator lives in its own seedance25.go file.
package calculators

import (
	"math"
	"strings"

	"synapta/internal/modules/agency"
)

// ─── Seedance (token-based pricing) ─────────────────────────────

// SeedanceCalculator estimates cost for BytePlus Seedance using the official
// token-based formula:
//
//	TokenUsage = (inputDuration + outputDuration) x width x height x FPS / 1024
//	MinTokens  = if input exists: (MAX(ceil(output*2/3), input) + output) x W x H x FPS / 1024
//	             else: same as TokenUsage
//	Price      = UnitPrice / 1,000,000 x MAX(TokenUsage, MinTokens)
//
// Unit prices (USD per million tokens):
//
//	dreamina-seedance-2-0-260128:
//	  With Video: 480p/720p = 4.30 | 1080p = 4.70
//	  Without:    480p/720p = 7.00 | 1080p = 7.70
type SeedanceCalculator struct{}

// NewSeedanceCalculator builds the calculator.
func NewSeedanceCalculator() *SeedanceCalculator { return &SeedanceCalculator{} }

// Name returns the calculator name.
func (c *SeedanceCalculator) Name() string { return "seedance" }

// Match reports whether this calculator handles the model.
func (c *SeedanceCalculator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "dreamina-seedance")
}

// CalculateFromResponse never extracts cost from the API response.
func (c *SeedanceCalculator) CalculateFromResponse(raw interface{}, req *agency.GeneratorRequest) (float64, bool) {
	return 0, false
}

// CalculateEstimated computes the estimated cost from request parameters.
func (c *SeedanceCalculator) CalculateEstimated(req *agency.GeneratorRequest) float64 {
	unitPrice := seedanceUnitPrice(req)
	if unitPrice == 0 {
		return 0
	}

	fps := 24.0
	width, height := seedanceDimensions(req.Resolution, req.Ratio)

	hasInputVideo := false
	inputDuration := req.InputDuration
	if inputDuration < 0 {
		inputDuration = 0
	}
	for _, item := range req.Content {
		if item.Type == "video" || item.Type == "image" {
			hasInputVideo = true
			break
		}
	}

	outputDuration := float64(req.Duration)
	if outputDuration <= 0 {
		outputDuration = 5
	}

	tokenUsage := (inputDuration + outputDuration) * width * height * fps / 1024

	var minTokens float64
	if hasInputVideo {
		effectiveInput := math.Ceil(outputDuration * 2.0 / 3.0)
		if inputDuration > effectiveInput {
			effectiveInput = inputDuration
		}
		minTokens = (effectiveInput + outputDuration) * width * height * fps / 1024
	} else {
		minTokens = tokenUsage
	}

	usedTokens := tokenUsage
	if minTokens > usedTokens {
		usedTokens = minTokens
	}

	quantity := req.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	return unitPrice / 1_000_000 * usedTokens * float64(quantity)
}

// NeedsBackgroundCalc is false: everything is computed inline.
func (c *SeedanceCalculator) NeedsBackgroundCalc() bool { return false }

func seedanceUnitPrice(req *agency.GeneratorRequest) float64 {
	isFast := strings.Contains(strings.ToLower(req.Model), "fast")

	hasInputVideo := false
	for _, item := range req.Content {
		if item.Type == "video" || item.Type == "image" {
			hasInputVideo = true
			break
		}
	}

	switch {
	case !isFast && hasInputVideo:
		if req.Resolution == "1080p" {
			return 4.70
		}
		return 4.30 // 480p, 720p
	case !isFast && !hasInputVideo:
		if req.Resolution == "1080p" {
			return 7.70
		}
		return 7.00 // 480p, 720p
	case isFast && hasInputVideo:
		return 3.30
	case isFast && !hasInputVideo:
		if req.Resolution == "720p" {
			return 6.60
		}
		return 5.80 // 480p
	default:
		return 0
	}
}

// seedanceDimensions returns (width, height) for a resolution and ratio.
func seedanceDimensions(resolution, ratio string) (float64, float64) {
	isVertical := strings.Contains(ratio, "9:16")
	isSquare := ratio == "1:1"
	is43 := ratio == "4:3"
	is34 := ratio == "3:4"
	is219 := ratio == "21:9"

	switch resolution {
	case "480p":
		switch {
		case isVertical:
			return 480, 854
		case isSquare:
			return 640, 640
		case is43:
			return 640, 480
		case is34:
			return 480, 640
		case is219:
			return 1120, 480
		default: // 16:9
			return 854, 480
		}
	case "1080p":
		switch {
		case isVertical:
			return 1080, 1920
		case isSquare:
			return 1080, 1080
		case is43:
			return 1440, 1080
		case is34:
			return 1080, 1440
		case is219:
			return 2520, 1080
		default: // 16:9
			return 1920, 1080
		}
	default: // 720p
		switch {
		case isVertical:
			return 720, 1280
		case isSquare:
			return 720, 720
		case is43:
			return 960, 720
		case is34:
			return 720, 960
		case is219:
			return 1680, 720
		default: // 16:9
			return 1280, 720
		}
	}
}

// ─── Seedream (per-image pricing) ───────────────────────────────

// SeedreamCalculator estimates cost for BytePlus image models (per image).
// Official prices (USD/image): seedream-5-0-lite 0.035, seedream-4-5 0.04,
// dreamina-seedream 0.04, seedream-4-0 0.03, seededit-3-0 0.03.
type SeedreamCalculator struct {
	models map[string]float64
}

// NewSeedreamCalculator builds the calculator.
func NewSeedreamCalculator() *SeedreamCalculator {
	return &SeedreamCalculator{
		models: map[string]float64{
			"seedream-5-0-lite": 0.035,
			"seedream-4-5":      0.04,
			"dreamina-seedream": 0.04,
			"seedream-4-0":      0.03,
			"seededit-3-0":      0.03,
		},
	}
}

// Name returns the calculator name.
func (c *SeedreamCalculator) Name() string { return "seedream" }

// Match reports whether this calculator handles the model.
func (c *SeedreamCalculator) Match(modelName string) bool {
	lower := strings.ToLower(modelName)
	for prefix := range c.models {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// CalculateFromResponse never extracts cost from the API response.
func (c *SeedreamCalculator) CalculateFromResponse(raw interface{}, req *agency.GeneratorRequest) (float64, bool) {
	return 0, false
}

// CalculateEstimated computes price × quantity.
func (c *SeedreamCalculator) CalculateEstimated(req *agency.GeneratorRequest) float64 {
	quantity := req.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	return c.priceForModel(req.Model) * float64(quantity)
}

// NeedsBackgroundCalc is false.
func (c *SeedreamCalculator) NeedsBackgroundCalc() bool { return false }

func (c *SeedreamCalculator) priceForModel(modelName string) float64 {
	lower := strings.ToLower(modelName)
	for prefix, price := range c.models {
		if strings.Contains(lower, prefix) {
			return price
		}
	}
	return 0.04
}

// ─── Gemini (free tier) ─────────────────────────────────────────

// GeminiCalculator returns zero cost for Gemini models.
type GeminiCalculator struct{}

// NewGeminiCalculator builds the calculator.
func NewGeminiCalculator() *GeminiCalculator { return &GeminiCalculator{} }

// Name returns the calculator name.
func (c *GeminiCalculator) Name() string { return "gemini" }

// Match reports whether this calculator handles the model.
func (c *GeminiCalculator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "gemini")
}

// CalculateFromResponse never extracts cost from the API response.
func (c *GeminiCalculator) CalculateFromResponse(raw interface{}, req *agency.GeneratorRequest) (float64, bool) {
	return 0, false
}

// CalculateEstimated returns zero.
func (c *GeminiCalculator) CalculateEstimated(req *agency.GeneratorRequest) float64 { return 0 }

// NeedsBackgroundCalc is false.
func (c *GeminiCalculator) NeedsBackgroundCalc() bool { return false }

// ─── Default ────────────────────────────────────────────────────

// DefaultCalculator returns zero cost for models without specific pricing.
type DefaultCalculator struct{}

// NewDefaultCalculator builds the calculator.
func NewDefaultCalculator() *DefaultCalculator { return &DefaultCalculator{} }

// Name returns the calculator name.
func (c *DefaultCalculator) Name() string { return "default" }

// Match handles every model.
func (c *DefaultCalculator) Match(_ string) bool { return true }

// CalculateFromResponse never extracts cost from the API response.
func (c *DefaultCalculator) CalculateFromResponse(_ interface{}, _ *agency.GeneratorRequest) (float64, bool) {
	return 0, false
}

// CalculateEstimated returns zero.
func (c *DefaultCalculator) CalculateEstimated(_ *agency.GeneratorRequest) float64 { return 0 }

// NeedsBackgroundCalc is false.
func (c *DefaultCalculator) NeedsBackgroundCalc() bool { return false }
