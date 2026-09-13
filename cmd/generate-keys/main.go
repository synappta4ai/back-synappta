// Command generate-keys creates VAPID key pair and security environment
// variables for Synapta. Run with: go run ./cmd/generate-keys
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/SherClockHolmes/webpush-go"
)

func main() {
	fmt.Println("╔════════════════════════════════════════════════════════════╗")
	fmt.Println("║          Synapta — Environment Key Generator             ║")
	fmt.Println("╚════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// ── VAPID Keys ────────────────────────────────────────────
	vapidPrivateKey, vapidPublicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating VAPID keys: %v\n", err)
		os.Exit(1)
	}

	// ── JWT Secret (64 random bytes → base64) ─────────────────
	jwtSecret, err := randomBase64(64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating JWT secret: %v\n", err)
		os.Exit(1)
	}

	// ── Encryption Key (exactly 32 raw characters) ────────────
	encKey, err := randomString(32)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error generating encryption key: %v\n", err)
		os.Exit(1)
	}

	// ── Print results ─────────────────────────────────────────
	fmt.Println("# ─── Security ────────────────────────────────────────────────")
	fmt.Printf("JWT_SECRET=%s\n", jwtSecret)
	fmt.Printf("ENCRYPTION_KEY=%s\n", encKey)
	fmt.Println()
	fmt.Println("# ─── Web Push (VAPID) ───────────────────────────────────────")
	fmt.Printf("PUSH_VAPID_PUBLIC_KEY=%s\n", vapidPublicKey)
	fmt.Printf("PUSH_VAPID_PRIVATE_KEY=%s\n", vapidPrivateKey)
	fmt.Println("PUSH_VAPID_SUBJECT=mailto:admin@synapta.local")
	fmt.Println()

	// ── Frontend hint ─────────────────────────────────────────
	fmt.Println("# ═════════════════════════════════════════════════════════════")
	fmt.Println("# Frontend .env — copy this value:")
	fmt.Printf("# VITE_VAPID_PUBLIC_KEY=%s\n", vapidPublicKey)
	fmt.Println("# ═════════════════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("✅ Keys generated. Copy the values above into your .env files.")
}

// randomBase64 returns n cryptographically random bytes encoded as base64url.
func randomBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomString returns a cryptographically random alphanumeric string of exactly n characters.
func randomString(n int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b), nil
}
