package credential

import (
	"synapta/internal/secretbox"
)

// encrypt seals plaintext with AES-256-GCM using the 32-byte key.
func encrypt(key, plaintext []byte) (string, error) {
	return secretbox.Seal(key, string(plaintext))
}

// decrypt opens a base64 AES-256-GCM sealed value.
func decrypt(key []byte, sealed string) ([]byte, error) {
	plain, err := secretbox.Open(key, sealed)
	if err != nil {
		return nil, err
	}
	return []byte(plain), nil
}

// mask returns a masked representation of a secret for API responses.
func mask(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 6 {
		return "••••••"
	}
	return secret[:3] + "••••" + secret[len(secret)-2:]
}
