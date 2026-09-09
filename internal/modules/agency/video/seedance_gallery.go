// Seedance 2.0 gallery variant. Reuses SeedanceGenerator with the gallery
// flag, which switches Match() to the dreamina-seedance-2-0-gallery model.
package agencyvideo

import (
	"net/http"
	"time"
)

// NewSeedanceGalleryGenerator builds the gallery variant.
func NewSeedanceGalleryGenerator() *SeedanceGenerator {
	return &SeedanceGenerator{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		gallery:    true,
		label:      "seedance-gallery",
	}
}
