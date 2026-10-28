package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// RegisterLocalFile stores a file that the server itself produced (a rendered
// video) in the library. Unlike Upload it is not subject to the browser upload
// cap (maxUploadBytes) and skips content dedup: a render is always a new
// asset. The bytes go through the same backend (local disk or tenant bucket)
// so the result is indistinguishable from any other library file.
func (s *Service) RegisterLocalFile(srcPath, displayName, category string) (*UploadResult, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	if !validCategoryName(category) {
		return nil, fmt.Errorf("invalid category %q", category)
	}
	ext := strings.ToLower(filepath.Ext(srcPath))
	if ext == "" {
		return nil, fmt.Errorf("file has no extension")
	}
	if displayName == "" {
		displayName = filepath.Base(srcPath)
	}
	if filepath.Ext(displayName) == "" {
		displayName += ext
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read rendered file: %w", err)
	}
	mimeType := mimeForExt(ext)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	backend, err := s.uploadBackend()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	fullPath := fmt.Sprintf("%s/%s%s", category, uuid.New().String(), ext)
	duration := float64(0)
	if strings.HasPrefix(mimeType, "video/") {
		duration = MediaDuration(bytes.NewReader(data), displayName)
	}
	rec := &File{
		ID: uuid.New().String(), Filename: displayName, Path: fullPath,
		Size: int64(len(data)), MimeType: mimeType, Category: category,
		Format: ext[1:], Storage: "persistent", Backend: backend.Name(),
		Duration: duration, SHA256: hex.EncodeToString(sum[:]),
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	if err := backend.Put(ctx, fullPath, data, mimeType); err != nil {
		return nil, fmt.Errorf("failed to save file: %w", err)
	}
	if err := s.store.CreateFile(rec); err != nil {
		_ = backend.Delete(ctx, fullPath)
		return nil, fmt.Errorf("failed to create db record: %w", err)
	}
	reader := s.bucketReaderFor([]File{*rec})
	return &UploadResult{
		ID: rec.ID, Filename: displayName,
		URL:  s.readURL(rec, reader, false),
		Size: rec.Size, MimeType: mimeType, Format: rec.Format, Category: category,
	}, nil
}

// RegisterLocalFile is the Handler bridge used by other modules (render).
func (h *Handler) RegisterLocalFile(srcPath, displayName, category string) (*UploadResult, error) {
	return h.svc.RegisterLocalFile(srcPath, displayName, category)
}
