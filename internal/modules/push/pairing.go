package push

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// PairingTTL is how long a QR code stays scannable.
const PairingTTL = 10 * time.Minute

// PairingCode links a QR code to the user+tenant that generated it.
type PairingCode struct {
	Code       string    `json:"code"`
	TenantSlug string    `json:"-"`
	UserID     int64     `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// PairingStore persists one-time codes in the system schema (redeem is
// public, so the tenant pool is resolved from the code itself).
type PairingStore struct {
	db *sql.DB
}

func NewPairingStore(db *sql.DB) *PairingStore { return &PairingStore{db: db} }

// Create mints a code bound to a tenant+user.
func (s *PairingStore) Create(tenantSlug string, userID int64, ttl time.Duration) (*PairingCode, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("pairing entropy: %w", err)
	}
	code := hex.EncodeToString(raw[:])
	var expires time.Time
	err := s.db.QueryRow(`INSERT INTO push_pairing_codes (code, tenant_slug, user_id, expires_at)
		VALUES ($1, $2, $3, NOW() + ($4 * INTERVAL '1 second'))
		RETURNING expires_at`, code, tenantSlug, userID, int64(ttl/time.Second)).
		Scan(&expires)
	if err != nil {
		return nil, err
	}
	return &PairingCode{Code: code, TenantSlug: tenantSlug, UserID: userID, ExpiresAt: expires}, nil
}

// Peek returns the code owner without consuming it (for the public-key
// lookup the scanning phone needs before subscribing).
func (s *PairingStore) Claim(code string) (*PairingCode, error) {
	var pc PairingCode
	err := s.db.QueryRow(`UPDATE push_pairing_codes SET used_at = NOW()
		WHERE code = $1 AND used_at IS NULL AND expires_at > NOW()
		RETURNING code, tenant_slug, user_id`,
		code).Scan(&pc.Code, &pc.TenantSlug, &pc.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &pc, nil
}

// Peek returns the code owner without consuming it (for the public-key
// lookup the scanning phone needs before subscribing).
func (s *PairingStore) Peek(code string) (*PairingCode, error) {
	var pc PairingCode
	err := s.db.QueryRow(`SELECT code, tenant_slug, user_id FROM push_pairing_codes
		WHERE code = $1 AND used_at IS NULL AND expires_at > NOW()`,
		code).Scan(&pc.Code, &pc.TenantSlug, &pc.UserID)
	if err != nil {
		return nil, err
	}
	return &pc, nil
}

// ─── Service ────────────────────────────────────────────────────

// PairingService mints QR codes (authed) and redeems them (public) by
// registering the scanning device's push subscription without a login.
type PairingService struct {
	store   *PairingStore
	poolFor  func(tenantSlug string) (*sql.DB, error)
	encKey   string
	subject  string
}

func NewPairingService(sysDB *sql.DB, poolFor func(string) (*sql.DB, error)) *PairingService {
	return &PairingService{store: NewPairingStore(sysDB), poolFor: poolFor}
}

// WithKeys wires per-user VAPID issuance for the public-key lookup.
func (s *PairingService) WithKeys(encryptionKey, subject string) *PairingService {
	s.encKey, s.subject = encryptionKey, subject
	return s
}

// Mint creates a code for the user that shows the QR.
func (s *PairingService) Mint(tenantSlug string, userID int64) (*PairingCode, error) {
	if tenantSlug == "" || userID <= 0 {
		return nil, errors.New("missing tenant or user")
	}
	return s.store.Create(tenantSlug, userID, PairingTTL)
}

// PublicKeyFor returns the code owner's VAPID public key (auto-generated)
// so the scanning phone can subscribe before redeeming. Does not consume.
func (s *PairingService) PublicKeyFor(code string, subjectFallback string) (string, error) {
	if code == "" {
		return "", errors.New("missing code")
	}
	pc, err := s.store.Peek(code)
	if err != nil {
		return "", err
	}
	pool, err := s.poolFor(pc.TenantSlug)
	if err != nil {
		return "", fmt.Errorf("tenant pool: %w", err)
	}
	keys, err := NewUserKeysStore(pool, s.encKey).Ensure(pc.UserID, subjectFallback, false)
	if err != nil {
		return "", err
	}
	return keys.PublicKey, nil
}

// Redeem validates the code and stores the device subscription in the
// owner's tenant schema. Single-use: replays get sql.ErrNoRows.
func (s *PairingService) Redeem(code string, req *SubscriptionRequest, userAgent string) error {
	if code == "" || req == nil || req.Endpoint == "" {
		return errors.New("code and subscription are required")
	}
	pc, err := s.store.Claim(code)
	if err != nil {
		return err
	}
	pool, err := s.poolFor(pc.TenantSlug)
	if err != nil {
		return fmt.Errorf("tenant pool: %w", err)
	}
	return NewStore(pool).Create(&Subscription{
		UserID: pc.UserID, Endpoint: req.Endpoint,
		P256DH: req.Keys.P256DH, Auth: req.Keys.Auth, UserAgent: userAgent,
	})
}
