package agency

import (
	"bytes"
	"image"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/disintegration/imaging"
	// Decodificadores de entrada para los originales que llegan del proveedor.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/gen2brain/webp"
	_ "golang.org/x/image/webp"

	"synapta/config"
	"synapta/internal/utils"
)

const (
	outputThumbDir       = "thumbnails"
	outputPreviewDir     = "previews"
	outputThumbSize      = 480
	outputPreviewSize    = 1600
	outputThumbQuality   = 72
	outputPreviewQuality = 78
)

// El backfill recorre outputs una sola vez por proceso.
var backfillDerivativesOnce sync.Once

// derivativeRel maps "image/task_1.jpg" to "thumbnails/image/task_1.webp".
func derivativeRel(root, rel string) string {
	dir := filepath.ToSlash(filepath.Dir(rel))
	base := filepath.Base(rel)
	base = strings.TrimSuffix(base, filepath.Ext(base)) + ".webp"
	if dir == "." || dir == "" {
		return root + "/" + base
	}
	return root + "/" + dir + "/" + base
}

// outputRel returns the outputs-relative path of an output, when it is one of
// our own files. Bucket URLs keep the /outputs/ segment too.
func outputRel(out *OutputResource) string {
	prefix := config.OutPutUrl() + "/"
	if strings.HasPrefix(out.LocalURL, prefix) {
		return strings.TrimPrefix(out.LocalURL, prefix)
	}
	if i := strings.Index(out.URL, prefix); i >= 0 {
		return out.URL[i+len(prefix):]
	}
	return ""
}

// ensureOutputDerivatives writes a thumbnail and a preview for image outputs
// that live on local disk. It never modifies the original, so the provider
// input and the downloadable file stay byte-identical.
func (s *Core) ensureOutputDerivatives(result *GeneratorResult) {
	if result == nil || s.outputsDir == "" {
		return
	}
	for i := range result.Outputs {
		out := &result.Outputs[i]
		if mediaKindForOutput(out) != utils.MediaTypeImage {
			continue
		}
		rel := outputRel(out)
		if rel == "" {
			continue
		}
		srcPath := filepath.Join(s.outputsDir, filepath.FromSlash(rel))
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		src, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
		if err != nil {
			log.Printf("[outputs] derivative decode %s: %v", rel, err)
			continue
		}
		if out.ThumbnailURL == "" {
			out.ThumbnailURL = s.writeOutputDerivative(src, rel, outputThumbDir, outputThumbSize, outputThumbQuality)
		}
		if out.PreviewURL == "" {
			out.PreviewURL = s.writeOutputDerivative(src, rel, outputPreviewDir, outputPreviewSize, outputPreviewQuality)
		}
	}
}

// writeOutputDerivative resizes (never upscales) and encodes a WebP derivative.
// It returns the public /outputs URL, or "" when the source is not usable.
func (s *Core) writeOutputDerivative(src image.Image, rel, root string, size, quality int) string {
	dstRel := derivativeRel(root, rel)
	dstPath := filepath.Join(s.outputsDir, filepath.FromSlash(dstRel))
	if _, err := os.Stat(dstPath); err == nil {
		return config.OutPutUrl() + "/" + dstRel
	}
	width, height := src.Bounds().Dx(), src.Bounds().Dy()
	scaled := src
	if width > size || height > size {
		scaled = imaging.Fit(src, size, size, imaging.Lanczos)
	}
	var buf bytes.Buffer
	if err := webp.Encode(&buf, scaled, webp.Options{Quality: quality, Method: 4}); err != nil {
		log.Printf("[outputs] derivative encode %s: %v", dstRel, err)
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		log.Printf("[outputs] derivative mkdir %s: %v", dstRel, err)
		return ""
	}
	if err := os.WriteFile(dstPath, buf.Bytes(), 0644); err != nil {
		log.Printf("[outputs] derivative write %s: %v", dstRel, err)
		return ""
	}
	return config.OutPutUrl() + "/" + dstRel
}

// decorateOutput fills the derivative URLs of an output (used when reading
// logs written before these fields existed). Only returns a URL when the
// derivative file is already on disk.
func (s *Core) decorateOutput(out *OutputResource) {
	if out == nil || mediaKindForOutput(out) != utils.MediaTypeImage {
		return
	}
	rel := outputRel(out)
	if rel == "" {
		return
	}
	if out.ThumbnailURL == "" {
		if url := s.derivativeURLIfExists(rel, outputThumbDir); url != "" {
			out.ThumbnailURL = url
		}
	}
	if out.PreviewURL == "" {
		if url := s.derivativeURLIfExists(rel, outputPreviewDir); url != "" {
			out.PreviewURL = url
		}
	}
}

func (s *Core) derivativeURLIfExists(rel, root string) string {
	if s.outputsDir == "" {
		return ""
	}
	dstRel := derivativeRel(root, rel)
	dstPath := filepath.Join(s.outputsDir, filepath.FromSlash(dstRel))
	if _, err := os.Stat(dstPath); err != nil {
		return ""
	}
	return s.absoluteOutputURL(config.OutPutUrl() + "/" + dstRel)
}

// decorateLogs decorates outputs of a page of logs in place.
func (s *Core) decorateLogs(logs []GenerationLog) {
	for i := range logs {
		for j := range logs[i].Outputs {
			s.decorateOutput(&logs[i].Outputs[j])
		}
	}
}

// BackfillOutputDerivatives generates missing derivatives for every image
// already in outputsDir. Runs once per process, in the background.
func (s *Core) BackfillOutputDerivatives() {
	if s.outputsDir == "" {
		return
	}
	backfillDerivativesOnce.Do(func() {
		go func() {
			count, failed := 0, 0
			err := filepath.WalkDir(s.outputsDir, func(path string, entry os.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return nil
				}
				rel, relErr := filepath.Rel(s.outputsDir, path)
				if relErr != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				if strings.HasPrefix(rel, outputThumbDir+"/") || strings.HasPrefix(rel, outputPreviewDir+"/") {
					return nil
				}
				if !strings.HasPrefix(rel, outputThumbDir+"/") && !strings.HasPrefix(rel, outputPreviewDir+"/") {
					switch strings.ToLower(filepath.Ext(rel)) {
					case ".jpg", ".jpeg", ".png", ".webp":
					default:
						return nil
					}
					result := &GeneratorResult{Outputs: []OutputResource{{
						LocalURL: config.OutPutUrl() + "/" + rel,
						Type:     utils.MediaTypeImage,
					}}}
					s.ensureOutputDerivatives(result)
					if result.Outputs[0].ThumbnailURL != "" || result.Outputs[0].PreviewURL != "" {
						count++
					} else {
						failed++
					}
				}
				return nil
			})
			if err != nil {
				log.Printf("[outputs] derivative backfill: %v", err)
				return
			}
			if count > 0 || failed > 0 {
				log.Printf("[outputs] derivative backfill: %d imágenes con derivados, %d sin cambios", count, failed)
			}
		}()
	})
}
