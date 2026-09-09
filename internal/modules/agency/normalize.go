package agency

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	_ "golang.org/x/image/webp"

	"synapta/config"
)

// ─── Asset normalization (BytePlus limits) ─────────────────────

// imageDims reads an image's pixel dimensions from a local file.
func imageDims(path string) (w, h int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// imageNeedsFix reports whether the dimensions violate BytePlus limits.
func imageNeedsFix(w, h int) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	if h < BytePlusMinHeight || h > BytePlusMaxHeight {
		return true
	}
	aspect := float64(w) / float64(h)
	return aspect < BytePlusMinAspect || aspect > BytePlusMaxAspect
}

// normalizeImage reads the image at srcPath and, if it violates BytePlus
// limits, returns JPEG bytes of a normalized copy. Returns (nil, nil) when no
// fix is needed. `aspectFix` selects "crop" (trims edges) vs pad (default).
func normalizeImage(srcPath, aspectFix string) ([]byte, error) {
	w, h, ok := imageDims(srcPath)
	if !ok || !imageNeedsFix(w, h) {
		return nil, nil
	}

	src, err := imaging.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	if src.Bounds().Dy() < BytePlusMinHeight {
		src = imaging.Resize(src, 0, BytePlusMinHeight, imaging.Lanczos)
	} else if src.Bounds().Dy() > BytePlusMaxHeight {
		src = imaging.Resize(src, 0, BytePlusMaxHeight, imaging.Lanczos)
	}

	aspect := float64(src.Bounds().Dx()) / float64(src.Bounds().Dy())
	if aspect < BytePlusMinAspect {
		targetW := int(float64(src.Bounds().Dy()) * BytePlusMinAspect)
		if aspectFix == "crop" {
			src = imaging.CropCenter(src, targetW, src.Bounds().Dy())
		} else {
			canvas := imaging.New(targetW, src.Bounds().Dy(), color.Black)
			src = imaging.PasteCenter(canvas, src)
		}
	} else if aspect > BytePlusMaxAspect {
		targetH := int(float64(src.Bounds().Dx()) / BytePlusMaxAspect)
		if aspectFix == "crop" {
			src = imaging.CropCenter(src, src.Bounds().Dx(), targetH)
		} else {
			canvas := imaging.New(src.Bounds().Dx(), targetH, color.Black)
			src = imaging.PasteCenter(canvas, src)
		}
	}

	var buf bytes.Buffer
	if err := imaging.Encode(&buf, src, imaging.JPEG, imaging.JPEGQuality(90)); err != nil {
		return nil, fmt.Errorf("failed to encode normalized image: %w", err)
	}
	return buf.Bytes(), nil
}

// ─── Output helpers ────────────────────────────────────────────

// resolveOutputURL turns a generator output URL (possibly relative) into an
// absolute URL that can be fetched server-side.
func resolveOutputURL(url, baseURL string) string {
	if url == "" {
		return ""
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	if strings.HasPrefix(url, "/") {
		return baseURL + url
	}
	return baseURL + "/outputs/" + url
}

// downloadBytes fetches a URL and returns its body.
func downloadBytes(url string) ([]byte, error) {
	if url == "" {
		return nil, fmt.Errorf("empty url")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// renameOutputFile renames a locally-downloaded output file to follow the
// pattern: {EventName}_{PieceCode}_G{gen}_{user}_{datetime}.mp4
func renameOutputFile(outputsDir, localURL, eventName, pieceCode string, generationNumber int, userHandle string) string {
	if localURL == "" || pieceCode == "" {
		return ""
	}

	ext := filepath.Ext(localURL)
	if ext == "" {
		ext = ".mp4"
	}
	now := time.Now()
	ts := now.Format("20060102_150405")

	safe := func(s string) string {
		r := strings.NewReplacer("/", "_", " ", "_", ":", "_")
		return r.Replace(s)
	}

	userPart := ""
	if userHandle != "" && userHandle != "0" && userHandle != "u0" {
		userPart = "_" + userHandle
	}

	prefix := safe(eventName)
	if prefix != "" {
		prefix += "_"
	}
	newName := fmt.Sprintf("%s%s_G%d%s_%s%s",
		prefix, safe(pieceCode), generationNumber, userPart, ts, ext)
	oldPath := filepath.Join(outputsDir, filepath.Base(localURL))
	newPath := filepath.Join(outputsDir, newName)

	if err := os.Rename(oldPath, newPath); err != nil {
		return ""
	}
	return config.OutPutUrl() + "/" + newName
}

// intPtrOrNil returns a pointer to v if v > 0, otherwise nil.
func intPtrOrNil(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

// extractContentTypes returns a sorted, comma-separated list of unique content types.
func extractContentTypes(items []ContentItem) string {
	seen := make(map[string]bool)
	var types []string
	for _, item := range items {
		if item.Type != "" && !seen[item.Type] {
			seen[item.Type] = true
			types = append(types, item.Type)
		}
	}
	// sort.Strings equivalent (small slices)
	for i := 1; i < len(types); i++ {
		for j := i; j > 0 && types[j] < types[j-1]; j-- {
			types[j], types[j-1] = types[j-1], types[j]
		}
	}
	if len(types) == 0 {
		return ""
	}
	return strings.Join(types, ",")
}
