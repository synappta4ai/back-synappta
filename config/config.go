package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// Environment names.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

type Config struct {
	Env                 string
	Port                string
	UploadDir           string
	ThumbnailDir        string
	MaxFileSize         int64
	ThumbnailWidth      int
	ThumbnailHeight     int
	VisionThumbnailSize int
	MaxOutputTokens     int
	MaxVisionImages     int
	BaseURL             string
	AllowedExts         map[string]bool
	DatabaseURL         string
	JWTSecret           string
	EncryptionKey       string // 32-byte key for credential encryption at rest
	OutputsDir          string

	// Inference worker (brain-master Python gRPC server) for downloaded
	// models. Empty disables the downloaded-model side of the catalog.
	WorkerAddr           string
	WorkerTimeoutSeconds int

	// AgentServerURL is the standalone DCS Agent server (Inmobiliaria /
	// Cine workflows) that the /agent/chat endpoint proxies to.
	AgentServerURL string

	// Default tenant: created at startup if missing; the platform superadmin
	// gets a membership in it so tenant-scoped requests work out of the box.
	DefaultTenantSlug string
	DefaultTenantName string

	// Super admin seed (platform-level, lives in the system schema)
	SuperAdminUsername string
	SuperAdminPassword string
	SuperAdminName     string
	SuperAdminSurname  string
	SuperAdminUserName string
	SuperAdminEmail    string

	// Web Push (VAPID)
	PushVapidPublicKey  string
	PushVapidPrivateKey string
	PushVapidSubject    string

	// DB connection pool limits
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime int // seconds
}

// IsProduction reports whether the server runs in production mode.
func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

// Load reads the environment and validates required settings.
// In production, insecure defaults cause a fatal error (fail-fast);
// in development they fall back to safe local values.
func Load() *Config {
	env := strings.ToLower(os.Getenv("APP_ENV"))
	if env == "" {
		env = EnvDevelopment
	}
	prod := env == EnvProduction

	port := getEnv("PORT", "9099")

	uploadDir := getEnv("UPLOAD_DIR", "./uploads")

	baseURL := getEnv("URL_PUBLIC", "http://localhost:"+port)

	databaseURL := getEnv("DATABASE_URL", "")
	if databaseURL == "" {
		if prod {
			log.Fatal("[config] DATABASE_URL is required in production")
		}
		databaseURL = "postgres://synapta:synapta_pass@localhost:5432/synapta?sslmode=disable"
	}

	jwtSecret := getEnv("JWT_SECRET", "")
	if jwtSecret == "" {
		if prod {
			log.Fatal("[config] JWT_SECRET is required in production")
		}
		jwtSecret = "dev_only_jwt_secret_change_me"
	}

	encryptionKey := getEnv("ENCRYPTION_KEY", "")
	if len(encryptionKey) != 32 {
		if prod {
			log.Fatal("[config] ENCRYPTION_KEY must be exactly 32 characters in production")
		}
		encryptionKey = "dev_only_32_byte_encryption_key!!" // 32 bytes
		if len(encryptionKey) != 32 {
			log.Fatal("[config] dev ENCRYPTION_KEY template is not 32 bytes")
		}
	}

	corsOrigins := getEnv("CORS_ALLOW_ORIGINS", "")
	if prod && (corsOrigins == "" || corsOrigins == "*") {
		log.Fatal("[config] CORS_ALLOW_ORIGINS must list explicit origins in production ('*' is not allowed)")
	}

	superAdminPassword := getEnv("SUPER_ADMIN_PASSWORD", "")
	if superAdminPassword == "" {
		if prod {
			log.Fatal("[config] SUPER_ADMIN_PASSWORD is required in production")
		}
		superAdminPassword = "superadmin_pass_123"
	}

	outputsDir := getEnv("OUTPUTS_DIR", "./outputs")

	visionThumbnailSize := 1024
	if v := os.Getenv("VISION_THUMBNAIL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			visionThumbnailSize = n
		}
	}

	maxOutputTokens := 64000
	if v := os.Getenv("MAX_OUTPUT_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxOutputTokens = n
		}
	}

	maxVisionImages := 24
	if v := os.Getenv("MAX_VISION_IMAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxVisionImages = n
		}
	}

	dbMaxOpen, err := strconv.Atoi(getEnv("DB_MAX_OPEN_CONNS", "25"))
	if err != nil || dbMaxOpen <= 0 {
		dbMaxOpen = 25
	}
	dbMaxIdle, err := strconv.Atoi(getEnv("DB_MAX_IDLE_CONNS", "5"))
	if err != nil || dbMaxIdle <= 0 {
		dbMaxIdle = 5
	}
	dbConnMaxLifetime, err := strconv.Atoi(getEnv("DB_CONN_MAX_LIFETIME", "1800"))
	if err != nil || dbConnMaxLifetime <= 0 {
		dbConnMaxLifetime = 1800
	}

	workerTimeout, err := strconv.Atoi(getEnv("BM_WORKER_TIMEOUT_SECONDS", "6"))
	if err != nil || workerTimeout <= 0 {
		workerTimeout = 6
	}

	log.Printf("[config] env=%s port=%s CORS_ALLOW_ORIGINS=%s worker=%s",
		env, port, tern(corsOrigins != "", corsOrigins, "(dev default *)"),
		tern(os.Getenv("BM_WORKER_ADDR") != "", os.Getenv("BM_WORKER_ADDR"), "(not configured)"))

	return &Config{
		Env:             env,
		Port:            port,
		UploadDir:       uploadDir,
		ThumbnailDir:    uploadDir + "/thumbnails",
		MaxFileSize:     10 << 20,
		ThumbnailWidth:  300,
		ThumbnailHeight: 300,

		VisionThumbnailSize: visionThumbnailSize,
		MaxOutputTokens:     maxOutputTokens,
		MaxVisionImages:     maxVisionImages,

		BaseURL: baseURL,
		AllowedExts: map[string]bool{
			".jpg":  true,
			".jpeg": true,
			".png":  true,
			".gif":  true,
			".webp": true,
		},

		DatabaseURL:   databaseURL,
		JWTSecret:     jwtSecret,
		EncryptionKey: encryptionKey,
		OutputsDir:    outputsDir,

		WorkerAddr:           strings.TrimSpace(os.Getenv("BM_WORKER_ADDR")),
		WorkerTimeoutSeconds: workerTimeout,

		AgentServerURL: getEnv("AGENT_SERVER_URL", "http://localhost:3200"),

		DefaultTenantSlug: getEnv("DEFAULT_TENANT_SLUG", "synapta"),
		DefaultTenantName: getEnv("DEFAULT_TENANT_NAME", "Synapta"),

		SuperAdminUsername: getEnv("SUPER_ADMIN_USERNAME", "superadmin"),
		SuperAdminPassword: superAdminPassword,
		SuperAdminName:     os.Getenv("SUPER_ADMIN_NAME"),
		SuperAdminSurname:  os.Getenv("SUPER_ADMIN_SURNAME"),
		SuperAdminUserName: os.Getenv("SUPER_ADMIN_USER_NAME"),
		SuperAdminEmail:    os.Getenv("SUPER_ADMIN_EMAIL"),

		PushVapidPublicKey:  os.Getenv("PUSH_VAPID_PUBLIC_KEY"),
		PushVapidPrivateKey: os.Getenv("PUSH_VAPID_PRIVATE_KEY"),
		PushVapidSubject:    getEnv("PUSH_VAPID_SUBJECT", "mailto:admin@synapta.local"),

		DBMaxOpenConns:    dbMaxOpen,
		DBMaxIdleConns:    dbMaxIdle,
		DBConnMaxLifetime: dbConnMaxLifetime,
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func tern(ok bool, a, b string) string {
	if ok {
		return a
	}
	return b
}

// OutPutUrl returns the URL path prefix for serving generated outputs.
func OutPutUrl() string { return "/outputs" }

// ValidateEncryptionKey ensures the key is usable for AES-256.
func ValidateEncryptionKey(key string) error {
	if len(key) != 32 {
		return fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	return nil
}
