package utils

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"synapta/config"
)

var mimeTypes = map[string]string{
	".mp4":  "video/mp4",
	".avi":  "video/x-msvideo",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".webm": "video/webm",
	".flv":  "video/x-flv",
	".wmv":  "video/x-ms-wmv",
	".m4v":  "video/mp4",
	".3gp":  "video/3gpp",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".aac":  "audio/aac",
	".m4a":  "audio/mp4",
	".wma":  "audio/x-ms-wma",
	".opus": "audio/opus",
	".aiff": "audio/aiff",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".svg":  "image/svg+xml",
	".tiff": "image/tiff",
	".tif":  "image/tiff",
	".ico":  "image/x-icon",
}

const (
	ModeBinary = "binary"
	ModeURL    = "url"
	ModeBase64 = "base64"

	MediaTypeVideo = "video"
	MediaTypeAudio = "audio"
	MediaTypeImage = "image"
	MediaTypeOther = "other"
)

// DownloadFromURL downloads content from a URL and returns raw bytes.
func DownloadFromURL(rawURL string) ([]byte, error) {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to download from URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("URL returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return data, nil
}

// DecodeBase64 decodes a base64 string, stripping any data URI prefix if present.
func DecodeBase64(s string) ([]byte, error) {
	if idx := strings.Index(s, "base64,"); idx != -1 {
		s = s[idx+7:]
	}

	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty base64 data")
	}

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 data: %w", err)
	}

	return data, nil
}

// DetectMediaType detects the media type from a filename extension.
func DetectMediaType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".mp4", ".avi", ".mov", ".mkv", ".webm", ".flv", ".wmv", ".m4v", ".3gp", ".ts":
		return MediaTypeVideo
	case ".mp3", ".wav", ".ogg", ".flac", ".aac", ".m4a", ".wma", ".opus", ".aiff":
		return MediaTypeAudio
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg", ".tiff", ".tif", ".ico":
		return MediaTypeImage
	default:
		return MediaTypeOther
	}
}

// SaveToOutput saves raw bytes to the outputs directory.
// Uses DetectMediaType to place files in a subfolder: /{video,audio,image,other}/
func SaveToOutput(data []byte, filename string) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("cannot save empty data")
	}

	filename = sanitizeFilename(filename)
	if filename == "" {
		return "", fmt.Errorf("invalid filename after sanitization")
	}

	mediaType := DetectMediaType(filename)
	baseDir := filepath.Join(".", config.OutPutUrl())
	subDir := filepath.Join(baseDir, mediaType)

	fullPath := filepath.Join(subDir, filename)
	if err := os.MkdirAll(subDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %w", subDir, err)
	}

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return config.OutPutUrl() + "/" + mediaType + "/" + filename, nil
}

// SaveBinaryOutput saves raw binary data to the outputs directory.
func SaveBinaryOutput(data []byte, filename string) (string, error) {
	return SaveToOutput(data, filename)
}

// SaveURLOutput downloads a URL and saves its content to the outputs directory.
func SaveURLOutput(rawURL, filename string) (string, error) {
	data, err := DownloadFromURL(rawURL)
	if err != nil {
		return "", err
	}
	return SaveToOutput(data, filename)
}

// SaveBase64Output decodes base64 data and saves it to the outputs directory.
func SaveBase64Output(b64Data, filename string) (string, error) {
	data, err := DecodeBase64(b64Data)
	if err != nil {
		return "", err
	}
	return SaveToOutput(data, filename)
}

// SaveOutput handles all three input modes in one call.
func SaveOutput(input interface{}, mode, filename string) (string, error) {
	switch mode {
	case ModeBinary:
		data, ok := input.([]byte)
		if !ok {
			return "", fmt.Errorf("invalid input type for mode %q: expected []byte", mode)
		}
		return SaveBinaryOutput(data, filename)

	case ModeURL:
		s, ok := input.(string)
		if !ok {
			return "", fmt.Errorf("invalid input type for mode %q: expected string", mode)
		}
		return SaveURLOutput(s, filename)

	case ModeBase64:
		s, ok := input.(string)
		if !ok {
			return "", fmt.Errorf("invalid input type for mode %q: expected string", mode)
		}
		return SaveBase64Output(s, filename)

	default:
		return "", fmt.Errorf("unsupported mode: %q (use %q, %q, or %q)",
			mode, ModeBinary, ModeURL, ModeBase64)
	}
}

// sanitizeFilename removes path separators and dangerous characters.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "..", "")
	name = strings.TrimSpace(name)
	return name
}
