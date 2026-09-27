// Package file manages tenant-scoped file storage: uploads (with SHA-256
// content dedup), thumbnails, trash lifecycle and the nightly temp purge cron.
package file

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"
	"github.com/gin-gonic/gin"

	"github.com/google/uuid"
	"golang.org/x/image/webp"

	"synapta/internal/middleware"
	"synapta/internal/utils"
)

// File is one stored file row (tenant schema).
type File struct {
	ID        string     `json:"id"`
	Filename  string     `json:"filename"`
	Path      string     `json:"path"`
	Size      int64      `json:"size"`
	MimeType  string     `json:"mime_type"`
	Category  string     `json:"category"`
	Format    string     `json:"format"`
	Storage   string     `json:"storage"`
	Duration  float64    `json:"duration"`
	SHA256    string     `json:"sha256"`
	Trashed   bool       `json:"trashed"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at"`

	// Computed for API responses (never persisted): public, JWT-free URLs
	// for <img>/<video> tags and external AI APIs. Filled by the service.
	URL          string `json:"url,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	// Computed: IDs of the events (projects) this file is assigned to.
	// Empty when the resource is not linked to any project.
	ProjectIDs []string `json:"project_ids,omitempty"`
	// Computed: ingredients (character/location/prop) that reference this
	// file — shown as resource metadata in the admin gallery.
	Ingredients []FileIngredient `json:"ingredients,omitempty"`
}

// FileIngredient summarizes an ingredient referencing a file.
type FileIngredient struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

type PaginatedFiles struct {
	Items      []File `json:"items"`
	Total      int    `json:"total"`
	Page       int    `json:"page"`
	PageSize   int    `json:"pageSize"`
	TotalPages int    `json:"totalPages"`
	// Tenant that owns these files (filled for platform superadmin views).
	TenantSlug string `json:"tenant_slug,omitempty"`
}

// ─── Disk store ─────────────────────────────────────────────────

type Store struct {
	db        *sql.DB
	uploadDir string
}

func NewStore(db *sql.DB, uploadDir string) (*Store, error) {
	for _, dir := range []string{
		uploadDir + "/images", uploadDir + "/videos", uploadDir + "/audio", uploadDir + "/temp",
		uploadDir + "/trash/images", uploadDir + "/trash/videos", uploadDir + "/trash/audio", uploadDir + "/trash/temp",
		uploadDir + "/trash/thumbnails", uploadDir + "/thumbnails/images", uploadDir + "/thumbnails/videos",
		uploadDir + "/thumbnails/audio", uploadDir + "/thumbnails/temp",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create dir %s: %w", dir, err)
		}
	}
	return &Store{db: db, uploadDir: uploadDir}, nil
}

func (s *Store) SaveFile(data []byte, subpath string) error {
	fullPath := filepath.Join(s.uploadDir, subpath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(fullPath, data, 0644)
}

func (s *Store) DeleteFile(subpath string) error {
	return os.Remove(filepath.Join(s.uploadDir, subpath))
}

func (s *Store) MoveFile(srcSubpath, dstSubpath string) error {
	src := filepath.Join(s.uploadDir, srcSubpath)
	dst := filepath.Join(s.uploadDir, dstSubpath)
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (s *Store) FileExists(subpath string) bool {
	_, err := os.Stat(filepath.Join(s.uploadDir, subpath))
	return err == nil
}

func (s *Store) GenerateThumbnail(srcSubpath string, width, height int) (string, error) {
	return s.generateThumbnailAt(srcSubpath, width, height, "thumbnails")
}

func (s *Store) generateThumbnailAt(srcSubpath string, width, height int, thumbRoot string) (string, error) {
	srcPath := filepath.Join(s.uploadDir, srcSubpath)
	thumbName := thumbRoot + "/" + srcSubpath
	dstPath := filepath.Join(s.uploadDir, thumbName)

	if _, err := os.Stat(dstPath); err == nil {
		return dstPath, nil
	}

	src, err := decodeImage(srcPath)
	if err != nil {
		return "", fmt.Errorf("failed to decode image: %w", err)
	}

	dst := imaging.Fit(src, width, height, imaging.Lanczos)

	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create thumbnail directory: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(srcSubpath))
	switch ext {
	case ".jpg", ".jpeg":
		err = imaging.Save(dst, dstPath, imaging.JPEGQuality(80))
	case ".png":
		err = imaging.Save(dst, dstPath, imaging.PNGCompressionLevel(6))
	default:
		err = imaging.Save(dst, dstPath)
	}
	if err != nil {
		return "", fmt.Errorf("failed to save thumbnail: %w", err)
	}

	return dstPath, nil
}

func decodeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg":
		if img, err := jpeg.Decode(f); err == nil {
			return img, nil
		}
	case ".png":
		if img, err := png.Decode(f); err == nil {
			return img, nil
		}
	case ".gif":
		if img, err := gif.Decode(f); err == nil {
			return img, nil
		}
	case ".webp":
		if img, err := webp.Decode(f); err == nil {
			return img, nil
		}
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if img, _, err := image.Decode(f); err == nil {
		return img, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if img, err := webp.Decode(f); err == nil {
		return img, nil
	}

	return nil, fmt.Errorf("unsupported or invalid image format")
}

func (s *Store) RemoveThumbnail(srcSubpath string) error {
	thumbSubpath := "thumbnails/" + srcSubpath
	if s.FileExists(thumbSubpath) {
		return s.DeleteFile(thumbSubpath)
	}
	return nil
}

// ValidateImage checks that data decodes as a known image format.
func (s *Store) ValidateImage(data []byte) error {
	_, err := imaging.Decode(bytes.NewReader(data))
	return err
}

// ─── DB operations ────────────────────────────────────────────

const fileCols = `id, filename, path, size, mime_type, category, format, storage, duration, sha256, trashed, created_at, updated_at, deleted_at`

func scanFile(f *File, scanner interface {
	Scan(dest ...interface{}) error
}) error {
	return scanner.Scan(&f.ID, &f.Filename, &f.Path, &f.Size, &f.MimeType, &f.Category, &f.Format,
		&f.Storage, &f.Duration, &f.SHA256, &f.Trashed, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt)
}

// ─── Event (project) assignment M:N ────────────────────────────

// LinkEvent assigns a file to a project. Idempotent: re-linking an existing
// pair is a no-op.
func (s *Store) LinkEvent(fileID, eventID string) error {
	_, err := s.db.Exec(`INSERT INTO event_files (file_id, event_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, fileID, eventID)
	return err
}

// UnlinkEvent removes a file's assignment to a project.
func (s *Store) UnlinkEvent(fileID, eventID string) error {
	_, err := s.db.Exec(`DELETE FROM event_files WHERE file_id = $1 AND event_id = $2`, fileID, eventID)
	return err
}

// ProjectIDsByFiles returns, for each file, the event IDs it is assigned to.
func (s *Store) ProjectIDsByFiles(fileIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(fileIDs))
	if len(fileIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT file_id::text, event_id::text FROM event_files
		WHERE file_id = ANY($1::uuid[]) ORDER BY created_at`, fileIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var fileID, eventID string
		if err := rows.Scan(&fileID, &eventID); err != nil {
			return nil, err
		}
		out[fileID] = append(out[fileID], eventID)
	}
	return out, rows.Err()
}

// ListFilesByEvent returns the active (non-trashed, non-deleted) files
// assigned to a project, newest first, optionally narrowed by category.
func (s *Store) ListFilesByEvent(eventID, category string) ([]File, error) {
	where := `WHERE ef.event_id = $1 AND f.trashed = FALSE AND f.deleted_at IS NULL`
	args := []interface{}{eventID}
	if category != "" {
		where += ` AND f.category = $2`
		args = append(args, category)
	}
	rows, err := s.db.Query(`SELECT `+fileColsSelect("f.")+` FROM event_files ef
		JOIN files f ON f.id = ef.file_id `+where+` ORDER BY ef.created_at DESC, f.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var f File
		if err := scanFile(&f, rows); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// fileColsSelect prefixes fileCols column names with a table alias.
func fileColsSelect(alias string) string {
	cols := strings.Split(fileCols, ", ")
	for i, c := range cols {
		cols[i] = alias + c
	}
	return strings.Join(cols, ", ")
}

// IngredientsByFiles returns, for each file, the ingredients
// (character/location/prop) that reference it.
func (s *Store) IngredientsByFiles(fileIDs []string) (map[string][]FileIngredient, error) {
	out := make(map[string][]FileIngredient, len(fileIDs))
	if len(fileIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT icf.file_id::text, i.id::text, i.type, i.name
		FROM ingredient_files icf
		JOIN ingredients i ON i.id = icf.ingredient_id AND i.deleted_at IS NULL
		WHERE icf.file_id = ANY($1::uuid[])
		ORDER BY i.name`, fileIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var fileID string
		var ing FileIngredient
		if err := rows.Scan(&fileID, &ing.ID, &ing.Type, &ing.Name); err != nil {
			return nil, err
		}
		out[fileID] = append(out[fileID], ing)
	}
	return out, rows.Err()
}

func (s *Store) CreateFile(f *File) error {
	query := `INSERT INTO files (id, filename, path, size, mime_type, category, format, storage, duration, sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING created_at, updated_at`
	return s.db.QueryRow(query, f.ID, f.Filename, f.Path, f.Size, f.MimeType, f.Category, f.Format, f.Storage, f.Duration, f.SHA256).
		Scan(&f.CreatedAt, &f.UpdatedAt)
}

// FindActiveByHash returns the newest non-trashed, non-deleted file with the
// given content hash, or nil when there is no match. Legacy rows (sha256 = ”)
// never match.
func (s *Store) FindActiveByHash(sha string) (*File, error) {
	f := &File{}
	err := scanFile(f, s.db.QueryRow(`SELECT `+fileCols+` FROM files
		WHERE sha256 = $1 AND trashed = FALSE AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, sha))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Store) GetFileByID(id string) (*File, error) {
	f := &File{}
	err := scanFile(f, s.db.QueryRow(`SELECT `+fileCols+` FROM files WHERE id = $1`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return f, err
}

func (s *Store) ListFiles(category, storage string, trashed bool) ([]File, error) {
	deletedClause := "deleted_at IS NULL"
	if trashed {
		deletedClause = "deleted_at IS NOT NULL"
	}
	query := `SELECT ` + fileCols + ` FROM files WHERE trashed = $1 AND ` + deletedClause
	args := []interface{}{trashed}
	argIdx := 2

	if category != "" {
		query += fmt.Sprintf(" AND category = $%d", argIdx)
		args = append(args, category)
		argIdx++
	}
	if storage != "" {
		query += fmt.Sprintf(" AND storage = $%d", argIdx)
		args = append(args, storage)
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var f File
		if err := scanFile(&f, rows); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) ListFilesPage(page, pageSize int, category, storage, search, eventID string) (*PaginatedFiles, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}

	where := "WHERE trashed = false AND deleted_at IS NULL"
	args := []interface{}{}
	argIdx := 1

	if eventID != "" {
		where += fmt.Sprintf(" AND id IN (SELECT file_id FROM event_files WHERE event_id = $%d)", argIdx)
		args = append(args, eventID)
		argIdx++
	}
	if category != "" {
		where += fmt.Sprintf(" AND category = $%d", argIdx)
		args = append(args, category)
		argIdx++
	}
	if storage != "" {
		where += fmt.Sprintf(" AND storage = $%d", argIdx)
		args = append(args, storage)
		argIdx++
	}
	if search != "" {
		where += fmt.Sprintf(" AND filename ILIKE $%d", argIdx)
		args = append(args, "%"+search+"%")
		argIdx++
	}

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM files "+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	offset := (page - 1) * pageSize
	query := `SELECT ` + fileCols + ` FROM files ` + where +
		` ORDER BY created_at DESC LIMIT $` + fmt.Sprintf("%d", argIdx) + ` OFFSET $` + fmt.Sprintf("%d", argIdx+1)
	args = append(args, pageSize, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	files := []File{}
	for rows.Next() {
		var f File
		if err := scanFile(&f, rows); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	totalPages := (total + pageSize - 1) / pageSize
	return &PaginatedFiles{Items: files, Total: total, Page: page, PageSize: pageSize, TotalPages: totalPages}, nil
}

func (s *Store) SoftDeleteFile(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE files SET trashed = true, deleted_at = $1, updated_at = $1 WHERE id = $2`, now, id)
	return err
}

func (s *Store) RestoreFile(id string) error {
	_, err := s.db.Exec(`UPDATE files SET trashed = false, deleted_at = NULL, updated_at = NOW() WHERE id = $1`, id)
	return err
}

func (s *Store) HardDeleteFile(id string) error {
	_, err := s.db.Exec(`DELETE FROM files WHERE id = $1`, id)
	return err
}

func (s *Store) ListExpiredTemp(maxAge time.Duration) ([]File, error) {
	query := `SELECT ` + fileCols + ` FROM files WHERE storage = 'temp' AND deleted_at IS NULL AND created_at < $1`
	rows, err := s.db.Query(query, time.Now().Add(-maxAge))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var f File
		if err := scanFile(&f, rows); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ─── Service ────────────────────────────────────────────────────

type Service struct {
	store   *Store
	baseURL string
	// tenantSlug names the tenant in public serve URLs
	// (/t/:slug/files/:id/serve): <img>/<video> tags cannot send JWTs, so
	// file URLs handed to browsers or external AI APIs must be public.
	tenantSlug string
}

func NewService(store *Store, baseURL, tenantSlug string) *Service {
	return &Service{store: store, baseURL: baseURL, tenantSlug: tenantSlug}
}

// publicFileURL builds a JWT-free serve URL for browsers and external APIs.
func (s *Service) publicFileURL(id, suffix string) string {
	return s.baseURL + "/api/v1/t/" + s.tenantSlug + "/files/" + id + suffix
}

type UploadResult struct {
	ID           string `json:"id"`
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	Size         int64  `json:"size"`
	MimeType     string `json:"mime_type"`
	Format       string `json:"format"`
	Category     string `json:"category"`
	// Duplicate is true when the upload was skipped because an identical
	// (by SHA-256) active file already existed; the result points at it.
	Duplicate bool `json:"duplicate,omitempty"`
}

func (s *Service) Upload(data []byte, filename, category, storage string, force bool) (*UploadResult, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return nil, fmt.Errorf("file has no extension")
	}

	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Content dedup: identical bytes already stored (and not trashed/deleted)
	// short-circuit the upload unless the caller forces a new copy.
	sum := sha256.Sum256(data)
	contentHash := hex.EncodeToString(sum[:])
	if !force {
		existing, err := s.store.FindActiveByHash(contentHash)
		if err != nil {
			return nil, fmt.Errorf("dedup lookup failed: %w", err)
		}
		if existing != nil {
			thumbnailURL := ""
			if strings.HasPrefix(existing.MimeType, "image/") {
				thumbnailURL = s.publicFileURL(existing.ID, "/thumbnail")
			}
			format := existing.Format
			return &UploadResult{
				ID: existing.ID, Filename: existing.Filename,
				URL:  s.publicFileURL(existing.ID, "/serve"),
				Size: existing.Size, MimeType: existing.MimeType, Format: format,
				Category: existing.Category, ThumbnailURL: thumbnailURL,
				Duplicate: true,
			}, nil
		}
	}

	newFilename := uuid.New().String() + ext
	fullPath := fmt.Sprintf("%s/%s", category, newFilename)

	duration := float64(0)
	if strings.HasPrefix(mimeType, "video/") {
		savedPath := filepath.Join(s.store.uploadDir, fullPath)
		duration = ReadMediaDuration(savedPath)
	}

	file := &File{
		ID:       uuid.New().String(),
		Filename: filename,
		Path:     fullPath,
		Size:     int64(len(data)),
		MimeType: mimeType,
		Category: category,
		Format:   ext[1:],
		Storage:  storage,
		Duration: duration,
		SHA256:   contentHash,
	}

	if err := s.store.SaveFile(data, fullPath); err != nil {
		return nil, fmt.Errorf("failed to save file: %w", err)
	}

	if err := s.store.CreateFile(file); err != nil {
		s.store.DeleteFile(fullPath)
		return nil, fmt.Errorf("failed to create db record: %w", err)
	}

	var thumbnailURL string
	if strings.HasPrefix(mimeType, "image/") {
		if _, err := s.store.GenerateThumbnail(fullPath, 300, 300); err != nil {
			fmt.Printf("warning: failed to generate thumbnail for %s: %v\n", fullPath, err)
		} else {
			thumbnailURL = s.publicFileURL(file.ID, "/thumbnail")
		}
	}

	format := ext[1:]
	if format == "jpeg" {
		format = "jpg"
	}

	return &UploadResult{
		ID: file.ID, Filename: filename,
		URL:  s.publicFileURL(file.ID, "/serve"),
		Size: file.Size, MimeType: mimeType, Format: format, Category: category,
		ThumbnailURL: thumbnailURL,
	}, nil
}

func (s *Service) GetFile(id string) (*File, error) { return s.store.GetFileByID(id) }

func (s *Service) GetServePath(id string) (string, error) {
	f, err := s.store.GetFileByID(id)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("file not found")
	}
	if f.Trashed {
		return "", fmt.Errorf("file has been deleted")
	}
	return filepath.Join(s.store.uploadDir, f.Path), nil
}

func (s *Service) GetThumbnailPath(id string) (string, error) {
	f, err := s.store.GetFileByID(id)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("file not found")
	}
	if f.Trashed {
		return "", fmt.Errorf("file has been deleted")
	}
	return s.store.GenerateThumbnail(f.Path, 300, 300)
}

func (s *Service) SoftDelete(id string) error {
	f, err := s.store.GetFileByID(id)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("file not found")
	}
	if f.Trashed {
		return fmt.Errorf("file already in trash")
	}
	if err := s.store.MoveFile(f.Path, "trash/"+f.Path); err != nil {
		return fmt.Errorf("failed to move to trash: %w", err)
	}
	thumbSubpath := "thumbnails/" + f.Path
	if s.store.FileExists(thumbSubpath) {
		_ = s.store.MoveFile(thumbSubpath, "trash/thumbnails/"+f.Path)
	}
	return s.store.SoftDeleteFile(id)
}

func (s *Service) Restore(id string) error {
	f, err := s.store.GetFileByID(id)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("file not found")
	}
	if !f.Trashed {
		return fmt.Errorf("file is not in trash")
	}
	if err := s.store.MoveFile("trash/"+f.Path, f.Path); err != nil {
		return fmt.Errorf("failed to restore from trash: %w", err)
	}
	thumbTrashPath := "trash/thumbnails/" + f.Path
	if s.store.FileExists(thumbTrashPath) {
		_ = s.store.MoveFile(thumbTrashPath, "thumbnails/"+f.Path)
	}
	return s.store.RestoreFile(id)
}

func (s *Service) HardDelete(id string) error {
	f, err := s.store.GetFileByID(id)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("file not found")
	}

	subpath := f.Path
	if f.Trashed {
		subpath = "trash/" + f.Path
	}
	if s.store.FileExists(subpath) {
		if err := s.store.DeleteFile(subpath); err != nil {
			return fmt.Errorf("failed to delete file: %w", err)
		}
	}
	thumbSubpath := "thumbnails/" + f.Path
	if s.store.FileExists(thumbSubpath) {
		_ = s.store.DeleteFile(thumbSubpath)
	}
	return s.store.HardDeleteFile(id)
}

// DecorateOne fills the computed public URL fields of a single file.
func (s *Service) DecorateOne(f *File) *File {
	if f == nil {
		return f
	}
	f.URL = s.publicFileURL(f.ID, "/serve")
	if strings.HasPrefix(f.MimeType, "image/") {
		f.ThumbnailURL = s.publicFileURL(f.ID, "/thumbnail")
	}
	return f
}

// decorate fills the computed public URL fields for API responses.
func (s *Service) decorate(files []File) []File {
	for i := range files {
		files[i].URL = s.publicFileURL(files[i].ID, "/serve")
		if strings.HasPrefix(files[i].MimeType, "image/") {
			files[i].ThumbnailURL = s.publicFileURL(files[i].ID, "/thumbnail")
		}
	}
	return files
}

func (s *Service) ListFilesPage(page, pageSize int, category, storage, search, eventID string) (*PaginatedFiles, error) {
	pageData, err := s.store.ListFilesPage(page, pageSize, category, storage, search, eventID)
	if err != nil {
		return nil, err
	}
	s.decorate(pageData.Items)
	if err := s.attachComputed(pageData.Items); err != nil {
		return nil, err
	}
	pageData.TenantSlug = s.tenantSlug
	return pageData, nil
}

// attachComputed fills the computed metadata fields (project IDs and
// referencing ingredients) for each listed file.
func (s *Service) attachComputed(files []File) error {
	if len(files) == 0 {
		return nil
	}
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.ID
	}
	byFile, err := s.store.ProjectIDsByFiles(ids)
	if err != nil {
		return err
	}
	ingsByFile, err := s.store.IngredientsByFiles(ids)
	if err != nil {
		return err
	}
	for i := range files {
		files[i].ProjectIDs = byFile[files[i].ID]
		files[i].Ingredients = ingsByFile[files[i].ID]
	}
	return nil
}

// ListFilesByEvent returns the active files assigned to a project.
func (s *Service) ListFilesByEvent(eventID, category string) ([]File, error) {
	files, err := s.store.ListFilesByEvent(eventID, category)
	if err != nil {
		return nil, err
	}
	return s.decorate(files), nil
}

// LinkEvent assigns a file to a project.
func (s *Service) LinkEvent(fileID, eventID string) error { return s.store.LinkEvent(fileID, eventID) }

// UnlinkEvent removes a file's project assignment.
func (s *Service) UnlinkEvent(fileID, eventID string) error {
	return s.store.UnlinkEvent(fileID, eventID)
}

func (s *Service) ListFiles(category, storage string, trashed bool) ([]File, error) {
	files, err := s.store.ListFiles(category, storage, trashed)
	if err != nil {
		return nil, err
	}
	return s.decorate(files), nil
}

func (s *Service) ListTrash() ([]File, error) {
	files, err := s.store.ListFiles("", "", true)
	if err != nil {
		return nil, err
	}
	return s.decorate(files), nil
}

func (s *Service) PurgeExpiredTemp() error {
	files, err := s.store.ListExpiredTemp(30 * 24 * time.Hour)
	if err != nil {
		return err
	}
	for _, f := range files {
		s.store.DeleteFile(f.Path)
		thumbSubpath := "thumbnails/" + f.Path
		if s.store.FileExists(thumbSubpath) {
			_ = s.store.DeleteFile(thumbSubpath)
		}
		s.store.HardDeleteFile(f.ID)
	}
	return nil
}

// StartPurgeCron launches the nightly temp purge loop.
func (s *Service) StartPurgeCron() {
	go func() {
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
			time.Sleep(next.Sub(now))
			if err := s.PurgeExpiredTemp(); err != nil {
				fmt.Printf("[file-cron] purge error: %v\n", err)
			}
		}
	}()
}

// ─── HTTP ───────────────────────────────────────────────────────

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Upload handles POST /files/upload (multipart: file, category, storage)
// Upload handles POST /files/upload
//
// Form fields:
//   - file      (required): the multipart file
//   - category  (required): images | videos | audio | temp
//   - storage   (optional): persistent | temp (default persistent)
//   - force     (optional): "true"/"1" stores a new copy even if an identical
//     active file already exists (dedup bypass)
//   - event_id  (optional): project (event) to assign the resource to
func (h *Handler) Upload(c *gin.Context) {
	category := c.PostForm("category")
	if category == "" {
		utils.BadRequest(c, "category is required")
		return
	}
	storage := c.DefaultPostForm("storage", "persistent")
	force := c.PostForm("force") == "true" || c.PostForm("force") == "1"
	eventID := c.PostForm("event_id")

	file, err := c.FormFile("file")
	if err != nil {
		utils.BadRequest(c, "file field is required")
		return
	}
	f, err := file.Open()
	if err != nil {
		utils.InternalError(c, "failed to read file")
		return
	}
	defer f.Close()

	data := make([]byte, file.Size)
	if _, err := f.Read(data); err != nil {
		utils.InternalError(c, "failed to read file data")
		return
	}

	result, err := h.svc.Upload(data, file.Filename, category, storage, force)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	// Assign the resource to the requested project (also for dedup hits: the
	// user is linking this project to an existing identical resource).
	if eventID != "" {
		if linkErr := h.svc.LinkEvent(result.ID, eventID); linkErr != nil {
			utils.InternalError(c, "uploaded but failed to link to project: "+linkErr.Error())
			return
		}
	}
	if result.Duplicate {
		// Not a new resource — 200 with the existing file + duplicate flag.
		utils.Success(c, result)
		return
	}
	utils.Created(c, result)
}

// GetFile handles GET /files/:id
func (h *Handler) GetFile(c *gin.Context) {
	f, err := h.svc.GetFile(c.Param("id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if f == nil {
		utils.NotFound(c, "file not found")
		return
	}
	utils.Success(c, h.svc.DecorateOne(f))
}

// ServeFile handles GET /files/:id/serve
func (h *Handler) ServeFile(c *gin.Context) {
	path, err := h.svc.GetServePath(c.Param("id"))
	serveOrError(c, path, err)
}

// ServeThumbnail handles GET /files/:id/thumbnail
func (h *Handler) ServeThumbnail(c *gin.Context) {
	path, err := h.svc.GetThumbnailPath(c.Param("id"))
	serveOrError(c, path, err)
}

func serveOrError(c *gin.Context, path string, err error) {
	if err != nil {
		switch err.Error() {
		case "file not found":
			utils.NotFound(c, err.Error())
		case "file has been deleted":
			utils.Gone(c, err.Error())
		default:
			utils.InternalError(c, err.Error())
		}
		return
	}
	c.File(path)
}

// SoftDelete handles DELETE /files/:id
func (h *Handler) SoftDelete(c *gin.Context) {
	if err := h.svc.SoftDelete(c.Param("id")); err != nil {
		if err.Error() == "file not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "file moved to trash")
}

// Restore handles POST /files/:id/restore
func (h *Handler) Restore(c *gin.Context) {
	if err := h.svc.Restore(c.Param("id")); err != nil {
		if err.Error() == "file not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "file restored")
}

// HardDelete handles DELETE /files/:id/hard
func (h *Handler) HardDelete(c *gin.Context) {
	if err := h.svc.HardDelete(c.Param("id")); err != nil {
		if err.Error() == "file not found" {
			utils.NotFound(c, err.Error())
			return
		}
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "file permanently deleted")
}

// ListFiles handles GET /files
func (h *Handler) ListFiles(c *gin.Context) {
	files, err := h.svc.ListFiles(c.Query("category"), c.Query("storage"), c.Query("trashed") == "true")
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if files == nil {
		files = []File{}
	}
	utils.Success(c, files)
}

// ListFilesPage handles GET /files/page
//
// Optional filters: category, storage, q (filename search) and event_id
// (project assignment — only resources linked to that project).
func (h *Handler) ListFilesPage(c *gin.Context) {
	page, _ := atoiDefault(c.Query("page"), 1)
	pageSize, _ := atoiDefault(c.Query("pageSize"), 50)
	result, err := h.svc.ListFilesPage(page, pageSize, c.Query("category"), c.Query("storage"), c.Query("q"), c.Query("event_id"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, result)
}

// ListFilesByEvent handles GET /files/by-event/:eventId — the project's
// assigned resources (all active files linked via event_files).
func (h *Handler) ListFilesByEvent(c *gin.Context) {
	files, err := h.svc.ListFilesByEvent(c.Param("eventId"), c.Query("category"))
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	if files == nil {
		files = []File{}
	}
	utils.Success(c, files)
}

// LinkEvent handles PUT /files/:id/event/:eventId — assign a resource to a project.
func (h *Handler) LinkEvent(c *gin.Context) {
	if err := h.svc.LinkEvent(c.Param("id"), c.Param("eventId")); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "file linked to project")
}

// UnlinkEvent handles DELETE /files/:id/event/:eventId — unassign a resource from a project.
func (h *Handler) UnlinkEvent(c *gin.Context) {
	if err := h.svc.UnlinkEvent(c.Param("id"), c.Param("eventId")); err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Message(c, "file unlinked from project")
}

func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid number")
		}
		n = n*10 + int(r-'0')
	}
	if n == 0 {
		return def, nil
	}
	return n, nil
}

// Module registers file routes.
//
// Public serve URLs carry the tenant slug: /t/:slug/files/:id/serve —
// external AI APIs fetch these without a JWT, so the tenant must be explicit.
type Module struct {
	resolve       HandlerResolver
	publicResolve PublicResolver
}

// HandlerResolver supplies the tenant-scoped handler for each request.
type HandlerResolver func(c *gin.Context) (*Handler, bool)

// PublicResolver resolves a handler from the :slug route param (public routes).
type PublicResolver func(c *gin.Context) (*Handler, bool)

func NewModule(resolve HandlerResolver, publicResolve PublicResolver) *Module {
	return &Module{resolve: resolve, publicResolve: publicResolve}
}

func (m *Module) Name() string { return "files" }

func (m *Module) Register(rg *gin.RouterGroup, authMw, tenantMw, _ gin.HandlerFunc) {
	// Public serve routes (generated assets must be fetchable by external AI APIs).
	m.public(rg.GET, "/t/:slug/files/:id/serve", "ServeFile")
	m.public(rg.GET, "/t/:slug/files/:id/thumbnail", "ServeThumbnail")

	g := rg.Group("/files")
	g.Use(authMw, tenantMw)
	{
		g.POST("/upload", m.priv("Upload"))
		g.GET("/page", m.priv("ListFilesPage"))
		g.GET("/by-event/:eventId", m.priv("ListFilesByEvent"))
		g.GET("", m.priv("ListFiles"))
		g.GET("/:id", m.priv("GetFile"))
		g.PUT("/:id/event/:eventId", m.priv("LinkEvent"))
		g.DELETE("/:id/event/:eventId", m.priv("UnlinkEvent"))
		g.GET("/:id/serve", m.priv("ServeFile"))
		g.GET("/:id/thumbnail", m.priv("ServeThumbnail"))
		g.DELETE("/:id", m.priv("SoftDelete"))
		g.POST("/:id/restore", m.priv("Restore"))
		g.DELETE("/:id/hard", m.priv("HardDelete"))
	}
}

// public builds a handler that resolves the tenant by slug then dispatches.
func (m *Module) public(route func(string, ...gin.HandlerFunc) gin.IRoutes, path, method string) {
	route(path, middleware.RateLimit(5, 20), func(c *gin.Context) {
		hdl, ok := m.publicResolve(c)
		if !ok {
			return
		}
		dispatch(hdl, method, c)
	})
}

// priv builds a handler that resolves the tenant from the JWT context.
func (m *Module) priv(method string) gin.HandlerFunc {
	return func(c *gin.Context) {
		hdl, ok := m.resolve(c)
		if !ok {
			return
		}
		dispatch(hdl, method, c)
	}
}

// dispatch routes to the named handler method.
func dispatch(hdl *Handler, method string, c *gin.Context) {
	switch method {
	case "Upload":
		hdl.Upload(c)
	case "ListFilesPage":
		hdl.ListFilesPage(c)
	case "ListFilesByEvent":
		hdl.ListFilesByEvent(c)
	case "LinkEvent":
		hdl.LinkEvent(c)
	case "UnlinkEvent":
		hdl.UnlinkEvent(c)
	case "ListFiles":
		hdl.ListFiles(c)
	case "GetFile":
		hdl.GetFile(c)
	case "SoftDelete":
		hdl.SoftDelete(c)
	case "Restore":
		hdl.Restore(c)
	case "HardDelete":
		hdl.HardDelete(c)
	case "ServeFile":
		hdl.ServeFile(c)
	case "ServeThumbnail":
		hdl.ServeThumbnail(c)
	default:
		utils.InternalError(c, "unknown handler method: "+method)
	}
}
