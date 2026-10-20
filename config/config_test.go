package config

import "testing"

// Load() rebuilds the dev defaults when these are absent; this test file does
// not run godotenv, so .env is never loaded here.
func setLoadEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("DATABASE_URL", "postgres://synapta:synapta_pass@localhost:5432/synapta?sslmode=disable")
	t.Setenv("MAX_UPLOAD_BYTES", "")
}

func TestMaxUploadBytesDefaultAndOverride(t *testing.T) {
	setLoadEnv(t)

	if got := Load().MaxUploadBytes; got != 10<<20 {
		t.Errorf("default MaxUploadBytes = %d, want %d", got, 10<<20)
	}

	t.Setenv("MAX_UPLOAD_BYTES", "2048")
	if got := Load().MaxUploadBytes; got != 2048 {
		t.Errorf("MaxUploadBytes = %d, want 2048", got)
	}

	// Basura o valores no positivos caen al default en vez de romper el boot.
	for _, bad := range []string{"nope", "0", "-5"} {
		t.Setenv("MAX_UPLOAD_BYTES", bad)
		if got := Load().MaxUploadBytes; got != 10<<20 {
			t.Errorf("MaxUploadBytes con %q = %d, want default", bad, got)
		}
	}
}
