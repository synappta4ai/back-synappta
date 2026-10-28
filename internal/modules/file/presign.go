package file

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"synapta/internal/utils"
)

// ErrStorageDisabled is returned by PresignUpload when the deployment has no
// object storage configured (MINIO_* unset).
var ErrStorageDisabled = errors.New("object storage is not configured")

// mediaFamilies maps the extensions accepted for direct uploads to the library
// category the object lands in. Executable types (html, svg, js) are absent on
// purpose: the bucket serves objects from its own origin, so storing one would
// be a stored-XSS vector.
var mediaFamilies = map[string]string{
	".jpg":  "images",
	".jpeg": "images",
	".png":  "images",
	".gif":  "images",
	".webp": "images",
	".mp4":  "videos",
	".mov":  "videos",
	".webm": "videos",
	".mkv":  "videos",
	".mp3":  "audio",
	".wav":  "audio",
	".m4a":  "audio",
	".ogg":  "audio",
	".flac": "audio",
}

// mediaFamilyPrefix is the MIME top-level type for a library category.
var mediaFamilyPrefix = map[string]string{
	"images": "image",
	"videos": "video",
	"audio":  "audio",
}

// PresignRequest asks for a direct-to-bucket upload URL.
type PresignRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// PresignResult is everything the browser needs to PUT the bytes straight to
// the bucket and then read the object back.
type PresignResult struct {
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Key       string            `json:"key"`
	Bucket    string            `json:"bucket"`
	PublicURL string            `json:"public_url,omitempty"`
	ExpiresIn int               `json:"expires_in"`
	// MaxSize is advisory for the PUT (S3 does not enforce size on a presigned
	// PUT) but the presign call rejects larger declared sizes.
	MaxSize int64 `json:"max_size"`
}

// PresignUpload validates the request and signs a PUT URL for
// <tenant>/<category>/<yyyy>/<mm>/<uuid><ext>.
func (s *Service) PresignUpload(req PresignRequest) (*PresignResult, error) {
	presigner, err := s.presigner()
	if err != nil {
		return nil, err
	}
	if presigner == nil {
		return nil, ErrStorageDisabled
	}

	name := filepath.Base(strings.TrimSpace(req.Filename))
	if name == "" || name == "." {
		return nil, errors.New("filename is required")
	}
	ext := strings.ToLower(filepath.Ext(name))
	family, ok := mediaFamilies[ext]
	if !ok {
		return nil, fmt.Errorf("unsupported file type %q (allowed: %s)", ext, allowedExtensions())
	}

	if req.Size < 0 {
		return nil, errors.New("size must be >= 0")
	}
	if s.maxUploadBytes > 0 && req.Size > s.maxUploadBytes {
		return nil, fmt.Errorf("file too large: %d bytes (max %d)", req.Size, s.maxUploadBytes)
	}

	contentType := normalizeContentType(req.ContentType)
	if contentType == "" {
		contentType = normalizeContentType(mime.TypeByExtension(ext))
	}
	if contentType == "" {
		return nil, fmt.Errorf("content_type is required for %s", ext)
	}
	if prefix := mediaFamilyPrefix[family]; !strings.HasPrefix(contentType, prefix+"/") {
		return nil, fmt.Errorf("content_type %q does not match extension %s", contentType, ext)
	}

	now := time.Now().UTC()
	key := fmt.Sprintf("%s/%s/%s/%s%s",
		s.tenantSlug, family, now.Format("2006/01"), uuid.NewString(), ext)

	return &PresignResult{
		UploadURL: presigner.PresignPut(key, contentType, now),
		Method:    http.MethodPut,
		Headers:   map[string]string{"Content-Type": contentType},
		Key:       key,
		Bucket:    presigner.Bucket(),
		PublicURL: presigner.PublicURL(key),
		ExpiresIn: int(presigner.TTL().Seconds()),
		MaxSize:   s.maxUploadBytes,
	}, nil
}

// Presign handles POST /files/presign.
//
// Body: {"filename": "foto.png", "content_type": "image/png", "size": 12345}
// The response's upload_url must be PUT with the returned headers; the object
// key is namespaced per tenant so buckets stay isolated and lifecycle rules
// can target one tenant.
func (h *Handler) Presign(c *gin.Context) {
	var req PresignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "invalid body: "+err.Error())
		return
	}

	res, err := h.svc.PresignUpload(req)
	if err != nil {
		switch {
		case errors.Is(err, ErrStorageDisabled):
			// No bucket for this tenant: the front keeps using /files/upload
			// (local disk), so this is a normal state, not a failure.
			utils.Error(c, http.StatusServiceUnavailable, "this tenant has no bucket configured; use /files/upload")
		case errors.Is(err, ErrStorageConfig):
			utils.InternalError(c, err.Error())
		default:
			utils.BadRequest(c, err.Error())
		}
		return
	}
	utils.Success(c, res)
}

// normalizeContentType trims parameters (charset) and lowercases the value.
func normalizeContentType(ct string) string {
	ct = strings.TrimSpace(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.ToLower(ct)
}

// allowedExtensions renders the accepted extensions for error messages.
func allowedExtensions() string {
	exts := make([]string, 0, len(mediaFamilies))
	for ext := range mediaFamilies {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return strings.Join(exts, ", ")
}
