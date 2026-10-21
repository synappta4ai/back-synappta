// Shared helpers for generators that receive images as base64 instead of a
// hosted URL (OpenAI-compatible b64_json, RunPod worker output).
package agencyimage

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"synapta/config"
	"synapta/internal/modules/agency"
)

// imageExtension sniffs the container from the decoded bytes so a JPEG saved
// as .png does not break later thumbnailing.
func imageExtension(raw []byte) string {
	switch {
	case len(raw) >= 8 && raw[0] == 0x89 && raw[1] == 'P' && raw[2] == 'N' && raw[3] == 'G':
		return ".png"
	case len(raw) >= 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF:
		return ".jpg"
	case len(raw) >= 12 && string(raw[0:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		return ".webp"
	case len(raw) >= 3 && raw[0] == 'G' && raw[1] == 'I' && raw[2] == 'F':
		return ".gif"
	default:
		return ".png"
	}
}

// safeFileToken keeps a provider id usable in a filename.
func safeFileToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "image"
	}
	return b.String()
}

// SaveBase64Image decodes a provider base64 image (raw or data URL), writes it
// under the server's outputs directory and returns the served resource.
//
// The file is written where the /outputs route serves it, so the core can
// then download/publish it like any provider URL.
func SaveBase64Image(prefix string, index int, data string) (agency.OutputResource, error) {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return agency.OutputResource{}, fmt.Errorf("empty base64 image")
	}
	// Accept "data:image/png;base64,…" as well as the bare payload.
	if idx := strings.Index(trimmed, "base64,"); idx >= 0 {
		trimmed = trimmed[idx+len("base64,"):]
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// Some workers pad/encode with the URL alphabet.
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(trimmed, "="))
	}
	if err != nil {
		return agency.OutputResource{}, fmt.Errorf("decode base64 image: %w", err)
	}

	outputsDir := filepath.Join(".", config.OutPutUrl())
	if err := os.MkdirAll(outputsDir, 0755); err != nil {
		return agency.OutputResource{}, fmt.Errorf("create outputs dir: %w", err)
	}
	filename := fmt.Sprintf("%s_%d%s", safeFileToken(prefix), index, imageExtension(raw))
	path := filepath.Join(outputsDir, filename)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		return agency.OutputResource{}, fmt.Errorf("write image: %w", err)
	}
	return agency.OutputResource{
		URL:  config.OutPutUrl() + "/" + filename,
		Type: "image",
	}, nil
}

// ImageSizeForRatio maps a catalog ratio to the OpenAI images `size` value.
// Square is the safe default; the values stay within the sizes hosted
// providers accept for FLUX/SDXL-style models.
func ImageSizeForRatio(ratio string) string {
	switch ratio {
	case "16:9":
		return "1344x768"
	case "9:16":
		return "768x1344"
	case "4:3":
		return "1152x864"
	case "3:4":
		return "864x1152"
	case "3:2":
		return "1216x832"
	case "2:3":
		return "832x1216"
	case "5:4":
		return "1120x896"
	case "4:5":
		return "896x1120"
	case "21:9":
		return "1536x640"
	default:
		return "1024x1024"
	}
}

// ImageDimensionsForRatio returns width/height for workers that take numeric
// dimensions instead of an OpenAI `size` string.
func ImageDimensionsForRatio(ratio string) (int, int) {
	size := ImageSizeForRatio(ratio)
	var w, h int
	if _, err := fmt.Sscanf(size, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 1024, 1024
	}
	return w, h
}

// LooksLikeBase64Image reports whether s decodes to image bytes, so workers
// that return base64 inside opaque fields can be told apart from text.
func LooksLikeBase64Image(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 64 {
		return false
	}
	if idx := strings.Index(trimmed, "base64,"); idx >= 0 {
		trimmed = trimmed[idx+len("base64,"):]
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return false
	}
	if len(raw) < 8 {
		return false
	}
	ext := imageExtension(raw)
	return ext != ".png" || (raw[0] == 0x89 && raw[1] == 'P')
}
