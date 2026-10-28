package push

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"synapta/internal/secretbox"
)

// ValidVapidPublicKey reports whether s is a usable VAPID applicationServerKey:
// an uncompressed P-256 point (65 bytes starting with 0x04) in base64url.
// A 43-char key decodes to 32 bytes (a private scalar, not a point) and the
// browser rejects it in pushManager.subscribe.
func ValidVapidPublicKey(s string) bool {
	raw := s
	if m := len(raw) % 4; m != 0 {
		raw += strings.Repeat("=", 4-m)
	}
	b, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	return len(b) == 65 && b[0] == 4
}

// UserKeys is a user's own VAPID identity (private key in plaintext only in
// memory; at rest it lives sealed in user_vapid_keys).
type UserKeys struct {
	UserID     int64
	PublicKey  string
	PrivateKey string
	Subject    string
}

// UserKeysStore persists per-user VAPID keypairs in the tenant schema.
type UserKeysStore struct {
	db  *sql.DB
	key []byte
}

func NewUserKeysStore(db *sql.DB, encryptionKey string) *UserKeysStore {
	return &UserKeysStore{db: db, key: []byte(encryptionKey)}
}

// Get loads the user's keys (sql.ErrNoRows when never generated).
func (s *UserKeysStore) Get(userID int64) (*UserKeys, error) {
	var k UserKeys
	var sealed string
	err := s.db.QueryRow(`SELECT user_id, public_key, private_key_sealed, subject
		FROM user_vapid_keys WHERE user_id = $1`, userID).
		Scan(&k.UserID, &k.PublicKey, &sealed, &k.Subject)
	if err != nil {
		return nil, err
	}
	priv, err := secretbox.Open(s.key, sealed)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt vapid private key: %w", err)
	}
	k.PrivateKey = priv
	return &k, nil
}

// Ensure returns the user's keys, generating and storing a fresh pair on
// first use (the same generation as cmd/generate-keys, one keypair per
// user). force=true rotates the pair.
func (s *UserKeysStore) Ensure(userID int64, subjectFallback string, force bool) (*UserKeys, error) {
	if userID <= 0 {
		return nil, errors.New("missing user id")
	}
	if !force {
		// Auto-heal: rows minted while the GenerateVAPIDKeys return order was
		// swapped hold the private scalar in public_key (43 chars, rejected
		// by the browser). Treat them as missing so they regenerate below.
		if k, err := s.Get(userID); err == nil && k.PublicKey != "" && k.PrivateKey != "" && ValidVapidPublicKey(k.PublicKey) {
			return k, nil
		}
	}
	// GenerateVAPIDKeys returns (privateKey, publicKey): keep this order.
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, fmt.Errorf("generate vapid keys: %w", err)
	}
	subject := strings.TrimSpace(subjectFallback)
	if subject == "" {
		subject = "mailto:admin@synapta.local"
	}
	sealed, err := secretbox.Seal(s.key, priv)
	if err != nil {
		return nil, fmt.Errorf("cannot seal vapid private key: %w", err)
	}
	var k UserKeys
	err = s.db.QueryRow(`INSERT INTO user_vapid_keys (user_id, public_key, private_key_sealed, subject)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE
		SET public_key = EXCLUDED.public_key,
		    private_key_sealed = EXCLUDED.private_key_sealed,
		    updated_at = NOW()
		RETURNING user_id, public_key, subject`,
		userID, pub, sealed, subject).
		Scan(&k.UserID, &k.PublicKey, &k.Subject)
	if err != nil {
		return nil, err
	}
	k.PrivateKey = priv
	return &k, nil
}

// Touch updates updated_at (bookkeeping for rotation audits).
func (s *UserKeysStore) Touch(userID int64) {
	_, _ = s.db.Exec(`UPDATE user_vapid_keys SET updated_at = $2 WHERE user_id = $1`, userID, time.Now())
}
