// Package calculators provides CostCalculator implementations for each model family.
package calculators

import (
	"strings"

	"synapta/internal/modules/agency"
)

// ─── Seedance 2.5 (per-second pricing) ─────────────────────────

// Seedance25Calculator estimates cost for BytePlus Seedance 2.5 using the
// ModelArk per-second rates (billed per output second; input video adds
// its duration to the bill):
//
//	480p: 0.103 USD/s | 720p: 0.231 USD/s | 1080p: 0.569 USD/s
//
// Derived from the official 5s 16:9 examples (0.514 / 1.156 / 2.843 USD).
type Seedance25Calculator struct {
	rates map[string]float64 // USD per output second, by resolution
}

// NewSeedance25Calculator builds the calculator.
func NewSeedance25Calculator() *Seedance25Calculator {
	return &Seedance25Calculator{
		rates: map[string]float64{
			"480p":  0.103,
			"720p":  0.231,
			"1080p": 0.569,
		},
	}
}

// Name returns the calculator name.
func (c *Seedance25Calculator) Name() string { return "seedance25" }

// Match reports whether this calculator handles the model.
func (c *Seedance25Calculator) Match(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "dreamina-seedance-2-5")
}

// CalculateFromResponse never extracts cost from the API response.
func (c *Seedance25Calculator) CalculateFromResponse(raw interface{}, req *agency.GeneratorRequest) (float64, bool) {
	return 0, false
}

// CalculateEstimated computes the estimated cost from request parameters.
func (c *Seedance25Calculator) CalculateEstimated(req *agency.GeneratorRequest) float64 {
	rate, ok := c.rates[req.Resolution]
	if !ok {
		return 0
	}

	seconds := float64(req.Duration)
	if seconds <= 0 {
		seconds = 5
	}
	for _, item := range req.Content {
		if item.Type == "video" || item.Type == "image" {
			if req.InputDuration > 0 {
				seconds += req.InputDuration
			}
			break
		}
	}

	quantity := req.Quantity
	if quantity <= 0 {
		quantity = 1
	}
	return rate * seconds * float64(quantity)
}

// NeedsBackgroundCalc is false: everything is computed inline.
func (c *Seedance25Calculator) NeedsBackgroundCalc() bool { return false }
