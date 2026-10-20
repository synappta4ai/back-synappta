package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"synapta/internal/storage"
)

// Errors returned by the blob backends.
var (
	// ErrBlobNotFound is the backend-neutral "object does not exist".
	ErrBlobNotFound = errors.New("blob not found")
)

// blobBackend stores and retrieves file bytes. The upload flow picks it per
// tenant: local disk by default, the tenant's bucket when configured.
type blobBackend interface {
	// Name is persisted in files.backend ("local" | "s3") so a file keeps
	// resolving to where it was written even if the tenant changes its config.
	Name() string
	Put(ctx context.Context, subpath string, data []byte, contentType string) error
	Open(ctx context.Context, subpath string) (io.ReadCloser, error)
	Delete(ctx context.Context, subpath string) error
	Exists(ctx context.Context, subpath string) (bool, error)
	// ReadURL returns a presigned GET URL (bucket) or the disk path (local).
	ReadURL(ctx context.Context, subpath string) (string, error)
}

// ─── Local disk ─────────────────────────────────────────────────

type localBackend struct{ uploadDir string }

func (b localBackend) Name() string { return "local" }

func (b localBackend) path(subpath string) string {
	return filepath.Join(b.uploadDir, subpath)
}

func (b localBackend) Put(_ context.Context, subpath string, data []byte, _ string) error {
	full := b.path(subpath)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0644)
}

func (b localBackend) Open(_ context.Context, subpath string) (io.ReadCloser, error) {
	f, err := os.Open(b.path(subpath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrBlobNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (b localBackend) Delete(_ context.Context, subpath string) error {
	err := os.Remove(b.path(subpath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (b localBackend) Exists(_ context.Context, subpath string) (bool, error) {
	_, err := os.Stat(b.path(subpath))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (b localBackend) ReadURL(_ context.Context, subpath string) (string, error) {
	return b.path(subpath), nil
}

// ─── Tenant bucket (S3/MinIO) ───────────────────────────────────

// s3Backend talks to the tenant bucket with presigned URLs, so the server
// never needs a separate signed-header client. Objects are addressed by the
// same subpath the local backend would use (images/<uuid>.png).
type s3Backend struct {
	presigner *storage.Presigner
	client    *http.Client
}

// blobClient has no global timeout: transfers can be large, so callers pass a
// context with the deadline they need.
var blobClient = &http.Client{}

// uploadTimeout bounds a server-side transfer to/from the tenant bucket.
const uploadTimeout = 10 * time.Minute

// thumbnailPath is where a file's thumbnail lives in the same backend.
func thumbnailPath(subpath string) string { return "thumbnails/" + subpath }

func newS3Backend(p *storage.Presigner) *s3Backend {
	return &s3Backend{presigner: p, client: blobClient}
}

func (b *s3Backend) Name() string { return "s3" }

func (b *s3Backend) Put(ctx context.Context, subpath string, data []byte, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	url := b.presigner.PresignPut(subpath, contentType, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	res, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("put %s: %w", subpath, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("put %s: status %d (%s)", subpath, res.StatusCode, msg)
	}
	// Todo PUT exitoso de S3/MinIO devuelve ETag. Un endpoint que contesta 2xx
	// sin ETag no guardó el objeto (la consola web de MinIO responde 200 con su
	// SPA a cualquier path): fallar acá evita perder el archivo en silencio y
	// que la fila quede apuntando a un objeto inexistente.
	if res.Header.Get("ETag") == "" {
		return fmt.Errorf("put %s: el endpoint respondió %d sin ETag (no parece una API S3)", subpath, res.StatusCode)
	}
	return nil
}

func (b *s3Backend) Open(ctx context.Context, subpath string) (io.ReadCloser, error) {
	url := b.presigner.PresignGet(subpath, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", subpath, err)
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		return nil, ErrBlobNotFound
	}
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		return nil, fmt.Errorf("get %s: status %d (%s)", subpath, res.StatusCode, msg)
	}
	return res.Body, nil
}

func (b *s3Backend) Delete(ctx context.Context, subpath string) error {
	url := b.presigner.PresignDelete(subpath, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete %s: %w", subpath, err)
	}
	defer res.Body.Close()
	// 204 on success; 404 means it is already gone.
	if res.StatusCode == http.StatusNotFound || res.StatusCode/100 == 2 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return fmt.Errorf("delete %s: status %d (%s)", subpath, res.StatusCode, msg)
}

func (b *s3Backend) Exists(ctx context.Context, subpath string) (bool, error) {
	url := b.presigner.PresignHead(subpath, time.Now())
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	res, err := b.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("head %s: %w", subpath, err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("head %s: status %d", subpath, res.StatusCode)
	}
}

func (b *s3Backend) ReadURL(_ context.Context, subpath string) (string, error) {
	return b.presigner.PresignGet(subpath, time.Now()), nil
}

// ─── Selection ──────────────────────────────────────────────────

// backend resolves the backend that a stored file must be read from. A file
// recorded as 's3' needs the tenant bucket: if the tenant removed the
// configuration the file cannot be served until it is restored.
func (s *Service) backend(name string) (blobBackend, error) {
	switch name {
	case "", "local":
		return localBackend{uploadDir: s.store.uploadDir}, nil
	case "s3":
		p, err := s.presigner()
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("%w: el archivo vive en un bucket que el tenant ya no tiene configurado", ErrStorageConfig)
		}
		return newS3Backend(p), nil
	default:
		return nil, fmt.Errorf("unknown storage backend %q", name)
	}
}

// validCategoryName accepts one safe path segment: letters, digits, '_' and
// '-'. Categories in use: images, videos, audio, temp (plus the studio's typed
// reference slots such as character/location/props).
func validCategoryName(category string) bool {
	if category == "" || len(category) > 32 {
		return false
	}
	for i := 0; i < len(category); i++ {
		c := category[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// uploadBackend picks where NEW bytes go: the tenant bucket when configured and
// enabled, local disk otherwise.
func (s *Service) uploadBackend() (blobBackend, error) {
	p, err := s.presigner()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return localBackend{uploadDir: s.store.uploadDir}, nil
	}
	return newS3Backend(p), nil
}

// contentTypeFor picks the stored content type for a subpath.
func contentTypeFor(subpath, fallback string) string {
	if fallback != "" {
		return fallback
	}
	if ct := mimeForExt(filepath.Ext(subpath)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// mimeForExt keeps the bucket's served type in line with the extension.
func mimeForExt(ext string) string {
	ext = strings.ToLower(ext)
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".m4v":
		return "video/x-m4v"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".ogv":
		return "video/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".avif":
		return "image/avif"
	}
	return ""
}
