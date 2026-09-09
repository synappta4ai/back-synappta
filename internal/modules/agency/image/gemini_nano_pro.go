// Gemini Pro image generator (synchronous). Reuses GeminiNanoGenerator with
// the longer 180s timeout and the gemini-nano-pro label so Match() routes
// the gemini-3-pro-image-preview model here.
package agencyimage

import (
	"net/http"
	"time"
)

// NewGeminiProGenerator builds the Gemini Pro image generator.
func NewGeminiProGenerator() *GeminiNanoGenerator {
	return &GeminiNanoGenerator{httpClient: &http.Client{Timeout: 180 * time.Second}, label: "gemini-nano-pro"}
}
