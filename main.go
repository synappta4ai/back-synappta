// Synapta — event-oriented AI generation backend.
//
// Bootstrap: config → system DB → migrations → tenancy registry → superadmin
// seed → per-tenant bundle manager → module registry → HTTP server with
// graceful shutdown, health endpoints and a background reconciler.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"synapta/config"
	"synapta/internal/db"
	"synapta/internal/middleware"
	"synapta/internal/modules"
	"synapta/internal/modules/agency"
	agencyimage "synapta/internal/modules/agency/image"
	agencytext "synapta/internal/modules/agency/text"
	agencyvideo "synapta/internal/modules/agency/video"
	"synapta/internal/modules/agent"
	"synapta/internal/modules/assignment"
	"synapta/internal/modules/auth"
	"synapta/internal/modules/consent"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/event"
	"synapta/internal/modules/file"
	"synapta/internal/modules/ingredient"
	"synapta/internal/modules/infoproduct"
	mcpmodule "synapta/internal/modules/mcp"
	"synapta/internal/modules/model"
	"synapta/internal/modules/notify"
	"synapta/internal/modules/preset"
	"synapta/internal/modules/push"
	"synapta/internal/modules/skill"
	"synapta/internal/modules/tenant"
	"synapta/internal/modules/workerbridge"
	"synapta/internal/runtime"
	"synapta/internal/tenancy"
	"synapta/internal/utils"
	"synapta/internal/worker"
)

func main() {
	// Load .env (development convenience; production uses real env vars).
	_ = godotenv.Load()

	// Initialize GlitchTip (Sentry)
	glitchtipDSN := getEnvDefault("GLITCHTIP_DSN", getEnvDefault("SENTRY_DSN", ""))
	if glitchtipDSN != "" && glitchtipDSN != "off" && glitchtipDSN != "false" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              glitchtipDSN,
			Debug:            getEnvDefault("APP_ENV", "development") != "production",
			TracesSampleRate: 1.0,
			Environment:      getEnvDefault("APP_ENV", "development"),
		})
		if err != nil {
			log.Printf("[main] sentry.Init warning: %v", err)
		} else {
			log.Printf("[main] GlitchTip (Sentry) initialized successfully")
			defer sentry.Flush(2 * time.Second)

			// Enviar evento de prueba inicial y hacer flush inmediato para verificar en GlitchTip
			eventID := sentry.CaptureMessage("Synapta backend started (GlitchTip integration test)")
			sentry.Flush(2 * time.Second)
			log.Printf("[main] GlitchTip test event sent, event_id: %v", eventID)
		}
	}

	cfg := config.Load()

	// Mode for Gin:
	// 1. Respeta la variable de entorno GIN_MODE (ej. GIN_MODE=release en .env)
	// 2. Si no está definida, pasa a ReleaseMode si está en producción
	if ginMode := os.Getenv("GIN_MODE"); ginMode != "" {
		gin.SetMode(ginMode)
	} else if cfg.IsProduction() {
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
	// El opt-in de notificaciones vive en las prefs del usuario (sistema):
	// el core lo consulta antes de pushear o fanear a los canales.
	manager.SetNotificationsChecker(authSvc.NotificationsAllowed)

	// ─── GPU worker bridge (Fase A del plan distribuido) ──────
	// Tablas en el schema de sistema: el worker se autentica con su propio
	// token y el tenant se resuelve desde ahí. Si BM_WORKER_ADDR está seteado
	// (modo local/túnel), el generator downloaded usa el gRPC directo y el
	// bridge queda disponible igual para el panel "Tu GPU".
	wbStore := workerbridge.NewStore(sysDB)
	manager.SetWorkerBridge(wbStore)

	// Curaduría del catálogo (maestro + tenant): el veto del maestro y las
	// prefs del tenant ordenan/filtran listados y bloquean generación.
	masterPrefs := model.NewMasterPrefsStore(sysDB)
	manager.SetModelMasterPrefs(masterPrefs)

	// Object storage is per tenant (storage_config, managed over HTTP): there
	// is no global bucket, so a tenant without one keeps using local disk.

	// ─── Middleware ───────────────────────────────────────────
	authMw := middleware.Auth(cfg.JWTSecret)
	tenantMw := tenancy.Middleware(reg)
	adminMw := middleware.RequireRole(1)

	// ─── Inference worker (downloaded models, live catalog) ──
	// Stateless client: no persistent connection, no cached catalog. Every
	// read goes live to the worker; if BM_WORKER_ADDR is unset the
	// downloaded side of the model catalog is simply absent.
	model.SetWorkerClient(worker.NewClient())
	model.SetWorkerAddr(cfg.WorkerAddr)
	model.SetWorkerArtifactBase(cfg.WorkerArtifactBase)
	model.SetBridgeSource(wbStore)

	// ─── Router ───────────────────────────────────────────────
	router := gin.New()
	router.Use(gin.Logger(), requestID())
	// GlitchTip / Sentry panic recovery
	router.Use(func(c *gin.Context) {
		defer func() {
			if rvr := recover(); rvr != nil {
				sentry.CurrentHub().Recover(rvr)
				sentry.Flush(2 * time.Second)
				panic(rvr) // delega a gin.Recovery para responder HTTP 500
			}
		}()
		c.Next()
	})
	router.Use(gin.Recovery())
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// CORS (explicit origins; fail-fast config guarantees non-empty in prod).
	origins := strings.Split(getEnvDefault("CORS_ALLOW_ORIGINS", "http://localhost:5173,http://localhost:4200"), ",")
	for i := range origins {
		origins[i] = strings.TrimSpace(origins[i])
	}
	corsCfg := cors.Config{
		AllowOrigins:     origins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Tenant-Slug", "X-Request-ID"},
		ExposeHeaders:    []string{"X-Request-ID", "X-Worker-Source"},
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
	registry.Register(consent.NewModule(consent.NewHandler(consent.NewService(consent.NewStore(sysDB)))))
	tenantUsersHdl := tenant.NewTenantUsersHandler(sysDB)
	tenant.SetTenantUsersHandler(tenantUsersHdl)
	registry.Register(tenant.NewModule(tenant.NewHandler(tenant.NewService(tenant.NewStore(sysDB), provisioner, reg, cfg.EncryptionKey))))
	// Platform superadmin manages a tenant's bucket by id (/tenants/:id/storage),
	// so those routes need the runtime bundle of that tenant.
	tenant.SetTenantStorageResolver(func(id int64) (tenant.StorageAdmin, error) {
		return manager.FilesForTenantID(id)
	})
	modelModule := model.NewModule(func(c *gin.Context) (model.CredentialLister, bool) {
		hdl, ok := manager.CredentialsFor(c)
		if !ok || hdl == nil {
			return nil, false
		}
		return hdl, true
	}, cfg.OutputsDir)
	modelModule.SetMasterPrefsStore(masterPrefs)
	modelModule.SetTenantPrefsSource(func(c *gin.Context) (model.TenantPrefsAccess, bool) {
		pool, ok := manager.TenantPoolFor(c)
		if !ok {
			return nil, false
		}
		return model.NewTenantPrefsStore(pool), true
	})
	registry.Register(modelModule)

	// Tenant-scoped modules resolve their handler per request.
	registry.Register(event.NewModule(manager.EventsFor))
	registry.Register(file.NewModule(manager.FilesFor, manager.FileForSlug))
	registry.Register(ingredient.NewModule(manager.IngredientsFor))
	registry.Register(assignment.NewModule(manager.AssignmentsFor))
	registry.Register(preset.NewModule(manager.PresetsFor))
	registry.Register(skill.NewModule(manager.SkillsFor))
	// QR pairing: códigos de un solo uso para suscribir dispositivos sin login.
	pairingSvc := push.NewPairingService(sysDB, func(tenantSlug string) (*sql.DB, error) {
		return reg.Pool(tenantSlug)
	}).WithKeys(cfg.EncryptionKey, cfg.PushVapidSubject)
	manager.SetPairingService(pairingSvc)
	pushModule := push.NewModule(manager.PushFor).WithPairing(pairingSvc)
	registry.Register(pushModule)
	registry.Register(notify.NewModule(manager.NotifyFor))
	registry.Register(credential.NewModule(manager.CredentialsFor))
	registry.Register(agent.NewModule(manager.CredentialsFor, manager.EventsFor, manager.AgencyCoreFor, manager.FlowsFor, cfg.AgentServerURL))
	registry.Register(workerbridge.NewModule(wbStore))
	// VideoMachine "Infoproducto": analiza videos de referencia y compone el prompt.
	registry.Register(infoproduct.NewModule(manager.CredentialsFor, manager.FilesFor, manager.TenantPoolFor))

	// Agency core with modality routes attached.
	// Servidor MCP remoto: mismo middleware auth+tenant, tools del dominio.
	registry.Register(mcpmodule.NewModule(manager.EventsFor, manager.AgencyCoreFor))

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

	// ─── Login con selector de tenant (permisos por membresía) ─
	v1.POST("/auth/login-tenant", func(c *gin.Context) {
		var req struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
			TenantID int64  `json:"tenant_id" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			utils.BadRequest(c, err.Error())
			return
		}
		memberships, err := tenantUsersHdl.MembershipsForUserByUsername(req.Username)
		if err != nil {
			utils.InternalError(c, "internal server error")
			return
		}
		var perms []string
		for _, m := range memberships {
			if m.TenantID == req.TenantID {
				perms = m.Permissions
				break
			}
		}
		if perms == nil && len(memberships) > 0 {
			// No es miembro: LoginForTenant lo rechaza con credenciales inválidas.
			perms = []string{}
		}
		token, err := authSvc.LoginForTenant(req.Username, req.Password, req.TenantID, perms)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidCreds) {
				utils.Unauthorized(c, err.Error())
				return
			}
			utils.InternalError(c, "internal server error")
			return
		}
		utils.Success(c, token)
	})

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
		if cfg.WorkerAddr != "" {
			if err := model.WorkerPing(ctx); err != nil {
				c.JSON(http.StatusServiceUnavailable,
					gin.H{"status": "degraded", "error": "worker: " + err.Error()})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"service": "synapta", "status": "ok"})
	})

	// ─── GlitchTip / Sentry test endpoints ────────────────────
	router.GET("/debug/sentry-test", func(c *gin.Context) {
		hub := sentry.CurrentHub()
		if hub == nil || hub.Client() == nil {
			c.JSON(http.StatusOK, gin.H{
				"status":  "error",
				"error":   "Sentry client no está inicializado (revisa GLITCHTIP_DSN)",
				"dsn_set": glitchtipDSN != "",
			})
			return
		}
		eventID := sentry.CaptureMessage("Prueba manual de GlitchTip desde /debug/sentry-test")
		flushed := sentry.Flush(2 * time.Second)
		c.JSON(http.StatusOK, gin.H{
			"status":   "sent",
			"event_id": eventID,
			"flushed":  flushed,
			"message":  "Evento despachado a GlitchTip (si no aparece en el panel, verifica que el DSN sea válido en GlitchTip)",
		})
	})
	router.GET("/debug/sentry-panic", func(c *gin.Context) {
		panic("Prueba de panic capturado por GlitchTip desde /debug/sentry-panic")
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
	go workerbridge.ReconcileLoop(wbStore, stop)    // marca failed jobs de GPUs que murieron

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
}

// requestID middleware tags each request with an X-Request-ID.
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
