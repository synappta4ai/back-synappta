// Synapta — event-oriented AI generation backend.
//
// Bootstrap: config → system DB → migrations → tenancy registry → superadmin
// seed → per-tenant bundle manager → module registry → HTTP server with
// graceful shutdown, health endpoints and a background reconciler.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"

	"synapta/config"
	"synapta/internal/db"
	"synapta/internal/middleware"
	"synapta/internal/modules"
	"synapta/internal/modules/agency"
	agencyimage "synapta/internal/modules/agency/image"
	agencytext "synapta/internal/modules/agency/text"
	agencyvideo "synapta/internal/modules/agency/video"
	"synapta/internal/modules/assignment"
	"synapta/internal/modules/auth"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/event"
	"synapta/internal/modules/file"
	"synapta/internal/modules/ingredient"
	"synapta/internal/modules/model"
	"synapta/internal/modules/preset"
	"synapta/internal/modules/push"
	"synapta/internal/modules/skill"
	"synapta/internal/modules/tenant"
	"synapta/internal/runtime"
	"synapta/internal/tenancy"
)

func main() {
	// Load .env (development convenience; production uses real env vars).
	_ = godotenv.Load()

	cfg := config.Load()

	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	// ─── System DB + migrations ───────────────────────────────
	poolCfg := db.PoolConfig{
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxLifetime: time.Duration(cfg.DBConnMaxLifetime) * time.Second,
	}

	// Strip any pre-existing options/search_path before opening the system pool.
	systemURL := cfg.DatabaseURL
	sysDB, err := db.Open(systemURL, poolCfg)
	if err != nil {
		log.Fatalf("[main] system db: %v", err)
	}
	defer db.LogClose(sysDB, "system")

	reg := tenancy.NewRegistry(sysDB, systemURL, poolCfg)
	provisioner := tenancy.NewProvisioner(reg, "migrations/tenant")

	if err := provisioner.MigrateSystem("migrations/system"); err != nil {
		log.Fatalf("[main] system migrations: %v", err)
	}
	if err := provisioner.MigrateAllTenants(); err != nil {
		log.Fatalf("[main] tenant migrations: %v", err)
	}

	// ─── Superadmin seed (system schema) ──────────────────────
	authStore := auth.NewStore(sysDB)
	authSvc := auth.NewService(authStore, cfg.JWTSecret)
	authSvc.SetSuperAdminConfig(cfg.SuperAdminUsername, cfg.SuperAdminPassword,
		cfg.SuperAdminName, cfg.SuperAdminSurname, cfg.SuperAdminUserName, cfg.SuperAdminEmail)
	if err := authSvc.SeedSuperAdmin(); err != nil {
		log.Fatalf("[main] seed superadmin: %v", err)
	}

	// ─── Default tenant + superadmin membership ───────────────
	// Guarantees at least one tenant exists so the superadmin can issue
	// tenant-scoped requests (credentials, models, generation) out of the box.
	superadmin, err := authStore.GetUserByUsername(cfg.SuperAdminUsername)
	if err != nil {
		log.Fatalf("[main] lookup superadmin: %v", err)
	}
	var superadminID int64
	if superadmin != nil {
		superadminID = superadmin.ID
	}
	if err := provisioner.EnsureDefaultTenant(cfg.DefaultTenantSlug, cfg.DefaultTenantName, superadminID); err != nil {
		log.Fatalf("[main] default tenant: %v", err)
	}

	// ─── Per-tenant service graph ─────────────────────────────
	manager := runtime.NewManager(cfg, reg)

	// ─── Middleware ───────────────────────────────────────────
	authMw := middleware.Auth(cfg.JWTSecret)
	tenantMw := tenancy.Middleware(reg)
	adminMw := middleware.RequireRole(1)

	// ─── Router ───────────────────────────────────────────────
	if cfg.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery(), requestID())

	// CORS (explicit origins; fail-fast config guarantees non-empty in prod).
	origins := strings.Split(getEnvDefault("CORS_ALLOW_ORIGINS", "http://localhost:5173,http://localhost:3000"), ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	corsCfg := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Tenant-Slug", "X-Request-ID"},
		ExposeHeaders:    []string{"X-Request-ID"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
	if len(origins) == 1 && origins[0] == "*" {
		corsCfg.AllowCredentials = false // '*' + credentials is invalid per spec
	}
	router.Use(cors.New(corsCfg))

	v1 := router.Group("/api/v1")

	// ─── Modules ──────────────────────────────────────────────
	registry := modules.NewRegistry()
	registry.Register(auth.NewModule(auth.NewHandler(authSvc)))
	registry.Register(tenant.NewModule(tenant.NewHandler(tenant.NewService(tenant.NewStore(sysDB), provisioner, reg, cfg.EncryptionKey))))
	registry.Register(model.NewModule())

	// Tenant-scoped modules resolve their handler per request.
	registry.Register(event.NewModule(manager.EventsFor))
	registry.Register(file.NewModule(manager.FilesFor, manager.FileForSlug))
	registry.Register(ingredient.NewModule(manager.IngredientsFor))
	registry.Register(assignment.NewModule(manager.AssignmentsFor))
	registry.Register(preset.NewModule(manager.PresetsFor))
	registry.Register(skill.NewModule(manager.SkillsFor))
	registry.Register(push.NewModule(manager.PushFor))
	registry.Register(credential.NewModule(manager.CredentialsFor))

	// Agency core with modality routes attached.
	agencyModule := agency.NewModule(manager.AgencyCoreFor)
	agencyModule.AttachModality("video", func(rg *gin.RouterGroup) {
		agencyvideo.RegisterRoutes(rg, agencyvideo.NewService(manager.AgencyCoreFor))
	})
	agencyModule.AttachModality("image", func(rg *gin.RouterGroup) {
		agencyimage.RegisterRoutes(rg, agencyimage.NewService(manager.AgencyCoreFor))
	})
	agencyModule.AttachModality("text", func(rg *gin.RouterGroup) {
		agencytext.RegisterRoutes(rg, agencytext.NewService(manager.AgencyCoreFor))
	})
	registry.Register(agencyModule)

	registry.Setup(v1, authMw, tenantMw, adminMw)

	// ─── Health endpoints (outside /api/v1 auth) ──────────────
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "env": cfg.Env})
	})
	router.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		if err := provisioner.PingSystem(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "degraded", "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"service": "synapta", "status": "ok"})
	})

	// ─── Generated outputs (own server URL) ───────────────────
	// Locally stored generation outputs are served here; the front plays them
	// from our domain instead of the expiring signed provider URLs.
	router.Static(config.OutPutUrl(), cfg.OutputsDir)

	// ─── Swagger UI (docs) ────────────────────────────────────
	router.StaticFile("/openapi.json", "docs/openapi.json")
	router.GET("/docs", func(c *gin.Context) {
		c.File("docs/index.html")
	})
	router.GET("/docs/*filepath", func(c *gin.Context) {
		c.File("docs/" + c.Param("filepath"))
	})

	// ─── Graceful shutdown server ─────────────────────────────
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan struct{})
	go agency.ReconcileLoop(manager.EachCore, stop) // recovers orphaned tasks per tenant

	go func() {
		log.Printf("[main] synapta listening on :%s (env=%s)", cfg.Port, cfg.Env)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[main] server: %v", err)
		}
	}()

	// Wait for interrupt signal.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[main] shutdown signal received")

	close(stop) // stop the reconciler first

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[main] forced shutdown: %v", err)
	}
	reg.CloseAll()
	log.Println("[main] bye")
} // requestID middleware tags each request with an X-Request-ID.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
