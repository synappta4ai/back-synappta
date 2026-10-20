package file

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"synapta/internal/secretbox"
	"synapta/internal/storage"
	"synapta/internal/utils"
)

// ErrStorageConfig marks a tenant storage configuration that exists but cannot
// be used (corrupt secrets, unreadable row). Distinct from ErrStorageDisabled,
// which means the tenant chose local storage.
var ErrStorageConfig = errors.New("invalid storage configuration")

var (
	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRe     = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

// StorageConfig is the API view of a tenant's bucket. Secrets never leave the
// server: AccessKey is masked and HasSecret only reports presence.
type StorageConfig struct {
	Configured bool       `json:"configured"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Bucket     string     `json:"bucket,omitempty"`
	Region     string     `json:"region,omitempty"`
	PublicBase string     `json:"public_base,omitempty"`
	Enabled    bool       `json:"enabled"`
	AccessKey  string     `json:"access_key_id,omitempty"`
	HasSecret  bool       `json:"has_secret"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// StorageConfigRequest is the payload for saving a tenant bucket. On update the
// credentials may be omitted to keep the stored ones.
type StorageConfigRequest struct {
	Endpoint   string `json:"endpoint"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	AccessKey  string `json:"access_key_id"`
	SecretKey  string `json:"secret_access_key"`
	PublicBase string `json:"public_base"`
	Enabled    *bool  `json:"enabled"`
}

// storageConfigRow is the persisted row (credentials still sealed).
type storageConfigRow struct {
	Endpoint   string
	Bucket     string
	Region     string
	AccessKey  string
	SecretKey  string
	PublicBase string
	Enabled    bool
	UpdatedAt  time.Time
}

// ─── Store ──────────────────────────────────────────────────────

// GetStorageConfig returns the tenant's row, or nil when it stores locally.
func (s *Store) GetStorageConfig() (*storageConfigRow, error) {
	row := &storageConfigRow{}
	err := s.db.QueryRow(`
		SELECT endpoint, bucket, region, access_key, secret_key, public_base, enabled, updated_at
		FROM storage_config WHERE singleton`).
		Scan(&row.Endpoint, &row.Bucket, &row.Region, &row.AccessKey, &row.SecretKey,
			&row.PublicBase, &row.Enabled, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// SaveStorageConfig upserts the tenant's single configuration row.
func (s *Store) SaveStorageConfig(row *storageConfigRow) error {
	_, err := s.db.Exec(`
		INSERT INTO storage_config
			(singleton, endpoint, bucket, region, access_key, secret_key, public_base, enabled, created_at, updated_at)
		VALUES (TRUE, $1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (singleton) DO UPDATE SET
			endpoint    = EXCLUDED.endpoint,
			bucket      = EXCLUDED.bucket,
			region      = EXCLUDED.region,
			access_key  = EXCLUDED.access_key,
			secret_key  = EXCLUDED.secret_key,
			public_base = EXCLUDED.public_base,
			enabled     = EXCLUDED.enabled,
			updated_at  = NOW()`,
		row.Endpoint, row.Bucket, row.Region, row.AccessKey, row.SecretKey, row.PublicBase, row.Enabled)
	return err
}

// DeleteStorageConfig makes the tenant fall back to local disk storage.
func (s *Store) DeleteStorageConfig() error {
	_, err := s.db.Exec(`DELETE FROM storage_config WHERE singleton`)
	return err
}

// ─── Service ────────────────────────────────────────────────────

// StorageConfigRead returns the tenant's configuration with masked secrets.
func (s *Service) StorageConfigRead() (*StorageConfig, error) {
	row, err := s.store.GetStorageConfig()
	if err != nil {
		return nil, err
	}
	if row == nil {
		return &StorageConfig{Configured: false}, nil
	}
	accessKey, err := secretbox.Open(s.encKey, row.AccessKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorageConfig, err)
	}
	return &StorageConfig{
		Configured: true,
		Endpoint:   row.Endpoint,
		Bucket:     row.Bucket,
		Region:     row.Region,
		PublicBase: row.PublicBase,
		Enabled:    row.Enabled,
		AccessKey:  maskSecret(accessKey),
		HasSecret:  row.SecretKey != "",
		UpdatedAt:  &row.UpdatedAt,
	}, nil
}

// SaveStorageConfig validates the bucket (including a live HEAD against it),
// seals the credentials and persists the row.
func (s *Service) SaveStorageConfig(req StorageConfigRequest) (*StorageConfig, error) {
	norm, err := normalizeStorageRequest(req)
	if err != nil {
		return nil, err
	}

	existing, err := s.store.GetStorageConfig()
	if err != nil {
		return nil, err
	}
	accessKey, secretKey := norm.AccessKey, norm.SecretKey
	if existing != nil {
		// Omitted credentials keep the stored ones (the front never receives
		// them back, so an edit would otherwise wipe them).
		if accessKey == "" {
			accessKey, _ = secretbox.Open(s.encKey, existing.AccessKey)
		}
		if secretKey == "" {
			secretKey, _ = secretbox.Open(s.encKey, existing.SecretKey)
		}
	}
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("access_key_id and secret_access_key are required")
	}

	presigner, err := storage.New(storage.Config{
		Endpoint:   norm.Endpoint,
		Bucket:     norm.Bucket,
		Region:     norm.Region,
		AccessKey:  accessKey,
		SecretKey:  secretKey,
		PublicBase: norm.PublicBase,
	})
	if err != nil {
		return nil, err
	}
	if norm.Enabled {
		if !s.allowPrivateEndpoints {
			if err := guardPublicEndpoint(norm.Endpoint); err != nil {
				return nil, err
			}
		}
		if err := bucketProbe(presigner); err != nil {
			return nil, err
		}
	}

	sealedAccessKey, err := secretbox.Seal(s.encKey, accessKey)
	if err != nil {
		return nil, fmt.Errorf("seal access key: %w", err)
	}
	sealedSecretKey, err := secretbox.Seal(s.encKey, secretKey)
	if err != nil {
		return nil, fmt.Errorf("seal secret key: %w", err)
	}

	row := &storageConfigRow{
		Endpoint:   norm.Endpoint,
		Bucket:     norm.Bucket,
		Region:     norm.Region,
		AccessKey:  sealedAccessKey,
		SecretKey:  sealedSecretKey,
		PublicBase: norm.PublicBase,
		Enabled:    norm.Enabled,
	}
	if err := s.store.SaveStorageConfig(row); err != nil {
		return nil, err
	}
	return s.StorageConfigRead()
}

// DeleteStorageConfig removes the bucket config (tenant back to local disk).
func (s *Service) DeleteStorageConfig() error { return s.store.DeleteStorageConfig() }

// presigner returns the tenant's presigner, or nil when the tenant has no
// bucket configured/enabled and therefore stores files on local disk.
func (s *Service) presigner() (*storage.Presigner, error) {
	if s.presignerFn != nil {
		return s.presignerFn()
	}
	return s.tenantPresigner()
}

func (s *Service) tenantPresigner() (*storage.Presigner, error) {
	row, err := s.store.GetStorageConfig()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorageConfig, err)
	}
	if row == nil || !row.Enabled {
		return nil, nil
	}
	accessKey, err := secretbox.Open(s.encKey, row.AccessKey)
	if err != nil || accessKey == "" {
		return nil, fmt.Errorf("%w: no se pudo descifrar la access key del bucket", ErrStorageConfig)
	}
	secretKey, err := secretbox.Open(s.encKey, row.SecretKey)
	if err != nil || secretKey == "" {
		return nil, fmt.Errorf("%w: no se pudo descifrar la secret key del bucket", ErrStorageConfig)
	}
	p, err := storage.New(storage.Config{
		Endpoint:   row.Endpoint,
		Bucket:     row.Bucket,
		Region:     row.Region,
		AccessKey:  accessKey,
		SecretKey:  secretKey,
		PublicBase: row.PublicBase,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorageConfig, err)
	}
	return p, nil
}

// ─── HTTP ───────────────────────────────────────────────────────

// GetStorageConfig handles GET /files/storage. Any authenticated tenant member
// can read it (the front needs it to pick direct upload vs local upload);
// secrets come back masked.
func (h *Handler) GetStorageConfig(c *gin.Context) {
	cfg, err := h.svc.StorageConfigRead()
	if err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Success(c, cfg)
}

// SaveStorageConfig handles PUT /files/storage (tenant admin).
func (h *Handler) SaveStorageConfig(c *gin.Context) {
	var req StorageConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "invalid body: "+err.Error())
		return
	}
	cfg, err := h.svc.SaveStorageConfig(req)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	utils.Success(c, cfg)
}

// DeleteStorageConfig handles DELETE /files/storage (tenant admin).
func (h *Handler) DeleteStorageConfig(c *gin.Context) {
	if err := h.svc.DeleteStorageConfig(); err != nil {
		utils.InternalError(c, err.Error())
		return
	}
	utils.Message(c, "storage config removed; the tenant stores files locally")
}

// ─── Helpers ────────────────────────────────────────────────────

// normalizedStorage is the validated payload (Enabled already resolved).
type normalizedStorage struct {
	Endpoint   string
	Bucket     string
	Region     string
	AccessKey  string
	SecretKey  string
	PublicBase string
	Enabled    bool
}

// normalizeStorageRequest trims, validates and canonicalizes the payload.
func normalizeStorageRequest(req StorageConfigRequest) (normalizedStorage, error) {
	out := normalizedStorage{}
	ep, err := storage.ParseEndpoint(req.Endpoint)
	if err != nil {
		return out, err
	}
	out.Endpoint = ep.Scheme + "://" + ep.Host + ep.Path

	out.Bucket = strings.ToLower(strings.TrimSpace(req.Bucket))
	if !bucketNameRe.MatchString(out.Bucket) {
		return out, fmt.Errorf("invalid bucket %q: lowercase letters, digits, '.' and '-' (3-63 chars)", out.Bucket)
	}

	out.Region = strings.ToLower(strings.TrimSpace(req.Region))
	if out.Region == "" {
		out.Region = "us-east-1"
	}
	if !regionRe.MatchString(out.Region) {
		return out, fmt.Errorf("invalid region %q", out.Region)
	}

	if base := strings.TrimSpace(req.PublicBase); base != "" {
		u, err := storage.ParseEndpoint(base)
		if err != nil {
			return out, fmt.Errorf("public_base: %w", err)
		}
		out.PublicBase = u.Scheme + "://" + u.Host + u.Path
	}

	out.AccessKey = strings.TrimSpace(req.AccessKey)
	out.SecretKey = strings.TrimSpace(req.SecretKey)
	out.Enabled = req.Enabled == nil || *req.Enabled
	return out, nil
}

// guardPublicEndpoint rejects endpoints that resolve to loopback, private or
// link-local addresses: the tenant supplies the URL and the server connects to
// it, so a private target would turn the probe into an SSRF oracle against the
// internal network. STORAGE_ALLOW_PRIVATE_ENDPOINTS=true opts out (dev MinIO).
func guardPublicEndpoint(endpoint string) error {
	u, err := storage.ParseEndpoint(endpoint)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if err != nil {
		return fmt.Errorf("no se pudo resolver el host del endpoint: %w", err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("el endpoint apunta a una dirección privada (%s): el servidor no se conecta a la red interna", ip)
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

// bucketProbe is a variable so tests can skip the network round-trip.
var bucketProbe = probeBucket

// probeBucket checks the credentials and the bucket with a signed HEAD before
// persisting them: a bucket that does not answer would silently break uploads.
func probeBucket(p *storage.Presigner) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, p.HeadBucketURL(time.Now()), nil)
	if err != nil {
		return fmt.Errorf("no se pudo firmar la verificación del bucket: %w", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("no se pudo conectar con el bucket: %w", err)
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("el bucket no existe o no es visible con esas credenciales (404)")
	case http.StatusForbidden:
		return fmt.Errorf("el bucket rechazó las credenciales (403)")
	default:
		return fmt.Errorf("el bucket respondió %d al verificar el acceso", res.StatusCode)
	}
}

// maskSecret keeps 4 leading and 4 trailing characters for display.
func maskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return "••••••"
	}
	return secret[:4] + "••••" + secret[len(secret)-4:]
}
