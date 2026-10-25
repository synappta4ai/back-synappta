// Package runtime owns the per-tenant service graph.
//
// With schema-per-tenant multi-tenancy every module's stores/services must be
// built over the tenant's own connection pool. The Manager builds a Bundle per
// tenant lazily (first request touching that tenant), caches it, and resolves
// it per request from the tenant context injected by the tenancy middleware.
//
// Modules never import this package: each one declares the narrow resolver
// interface it needs (consumer-side interfaces), and main connects the
// Manager to the modules. This keeps the dependency graph acyclic.
package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/agency/calculators"
	agencyimage "synapta/internal/modules/agency/image"
	agencytext "synapta/internal/modules/agency/text"
	agencyvideo "synapta/internal/modules/agency/video"
	"synapta/internal/modules/assignment"
	"synapta/internal/modules/credential"
	"synapta/internal/modules/event"
	"synapta/internal/modules/file"
	"synapta/internal/modules/ingredient"
	"synapta/internal/modules/model"
	"synapta/internal/modules/preset"
	"synapta/internal/modules/push"
	"synapta/internal/modules/skill"
	"synapta/internal/modules/workerbridge"
	"synapta/internal/tenancy"
	"synapta/internal/toolkit"
	"synapta/internal/worker"
)

// Manager builds and caches per-tenant bundles.
type Manager struct {
	cfg *config.Config
	reg *tenancy.Registry

	// workerBridgeStore es el registry/cola de GPUs emparejadas (schema de
	// sistema). nil → los generators downloaded viven solo del gRPC directo.
	workerBridgeStore *workerbridge.Store

	// modelMaster es la curaduría de plataforma (schema de sistema). nil →
	// sin veto del maestro: solo aplican las prefs del tenant.
	modelMaster *model.MasterPrefsStore

	mu      sync.Mutex
	bundles map[string]*Bundle // slug → bundle
}

// Bundle is the complete service graph for one tenant.
type Bundle struct {
	Tenant *tenancy.Tenant

	// Handlers consumed by the modules (each wraps its per-tenant services).
	Events      *event.Handler
	Files       *file.Handler
	Ingredients *ingredient.Handler
	Assignments *assignment.Handler
	Presets     *preset.Handler
	Skills      *skill.Handler
	Push        *push.Handler
	Credentials *credential.Handler
	Agency      *agency.Handler

	// Flows persiste el estado del flujo /agency por (usuario, agente)
	// sobre agency_flows (schema del tenant).
	Flows toolkit.FlowsAPI

	agencyCore *agency.Core // for the reconciler and modality resolvers
}

// NewManager creates a bundle manager over the tenancy registry.
func NewManager(cfg *config.Config, reg *tenancy.Registry) *Manager {
	return &Manager{cfg: cfg, reg: reg, bundles: map[string]*Bundle{}}
}

// SetWorkerBridge habilita el camino saliente (GPU de usuarios detrás de
// NAT) en los generators downloaded de todos los tenants.
func (m *Manager) SetWorkerBridge(store *workerbridge.Store) {
	m.workerBridgeStore = store
}

// SetModelMasterPrefs wire la curaduría de plataforma (veto + badges) en
// listados y generación de todos los tenants.
func (m *Manager) SetModelMasterPrefs(store *model.MasterPrefsStore) {
	m.modelMaster = store
}

// TenantPoolFor returns the tenant-schema pool for the request's tenant.
// False means no tenant context, so per-tenant prefs are skipped.
func (m *Manager) TenantPoolFor(c *gin.Context) (*sql.DB, bool) {
	t := tenancy.FromContext(c.Request.Context())
	if t == nil {
		return nil, false
	}
	pool, err := m.reg.Pool(t.Slug)
	if err != nil {
		log.Printf("[runtime] tenant pool for model prefs %q: %v", t.Slug, err)
		return nil, false
	}
	return pool, true
}

// Resolve returns the bundle for the request's tenant (set by the tenancy
// middleware). On failure it writes the HTTP error and returns ok=false.
func (m *Manager) Resolve(c *gin.Context) (*Bundle, bool) {
	t := tenancy.FromContext(c.Request.Context())
	if t == nil {
		c.AbortWithStatusJSON(500, gin.H{"success": false, "message": "missing tenant context"})
		return nil, false
	}
	b, err := m.Bundle(t)
	if err != nil {
		log.Printf("[runtime] bundle for tenant %q: %v", t.Slug, err)
		c.AbortWithStatusJSON(500, gin.H{"success": false, "message": "tenant services unavailable"})
		return nil, false
	}
	return b, true
}

// FilesForTenantID resolves a tenant's file handler from its numeric id.
//
// Platform-superadmin endpoints that act on another tenant (/tenants/:id/...)
// have no tenant in their token, so they cannot use Resolve. A nil handler
// with a nil error means the tenant does not exist.
func (m *Manager) FilesForTenantID(id int64) (*file.Handler, error) {
	t, err := m.tenantByID(id)
	if err != nil || t == nil {
		return nil, err
	}
	b, err := m.Bundle(t)
	if err != nil {
		return nil, err
	}
	return b.Files, nil
}

// tenantByID reads one tenant row from the system schema (nil when absent).
func (m *Manager) tenantByID(id int64) (*tenancy.Tenant, error) {
	t := &tenancy.Tenant{}
	err := m.reg.System().QueryRow(
		`SELECT id, slug, name, active, created_at, updated_at FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Active, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Bundle returns (building if needed) the bundle for a tenant.
func (m *Manager) Bundle(t *tenancy.Tenant) (*Bundle, error) {
	m.mu.Lock()
	b, ok := m.bundles[t.Slug]
	m.mu.Unlock()
	if ok {
		return b, nil
	}

	built, err := m.build(t)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.bundles[t.Slug] = built
	m.mu.Unlock()
	return built, nil
}

// FileForSlug resolves the file handler for the tenant named by the :slug
// route param (public serve endpoints). Writes the HTTP error on failure.
func (m *Manager) FileForSlug(c *gin.Context) (*file.Handler, bool) {
	slug := c.Param("slug")
	if !tenancy.ValidSlug(slug) {
		c.AbortWithStatusJSON(404, gin.H{"success": false, "message": "unknown tenant"})
		return nil, false
	}
	t, err := m.reg.TenantBySlug(slug)
	if err != nil {
		log.Printf("[runtime] lookup tenant %q: %v", slug, err)
		c.AbortWithStatusJSON(500, gin.H{"success": false, "message": "tenant lookup failed"})
		return nil, false
	}
	if t == nil || !t.Active {
		c.AbortWithStatusJSON(404, gin.H{"success": false, "message": "unknown tenant"})
		return nil, false
	}
	b, err := m.Bundle(t)
	if err != nil {
		log.Printf("[runtime] bundle for tenant %q: %v", slug, err)
		c.AbortWithStatusJSON(500, gin.H{"success": false, "message": "tenant services unavailable"})
		return nil, false
	}
	return b.Files, true
}

// bridgeModelResolver arma el resolver de modelos del bridge para un tenant:
// busca el nombre en los snapshots de sus GPUs emparejadas (la única fuente
// viva cuando no hay worker central) y devuelve la modalidad resuelta, que el
// generator necesita para no despachar una imagen como video.
func bridgeModelResolver(store *workerbridge.Store, tenantID int64) agency.BridgeModelResolver {
	return func(ctx context.Context, name string) (*agency.BridgeModelRef, error) {
		models, err := store.SnapshotsForTenant(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if !strings.EqualFold(m.Key, name) {
				continue
			}
			return &agency.BridgeModelRef{
				Name:        m.Key,
				Label:       m.Label,
				Modality:    model.ModalityForWorkerMode(m.Mode),
				WorkerID:    m.WorkerID,
				WorkerName:  m.WorkerName,
				Available:   m.Available,
				Downloading: m.Downloading,
				SizeBytes:   m.SizeBytes,
			}, nil
		}
		return nil, nil // el catálogo de las GPUs no declara ese modelo
	}
}

// EachCore runs fn over every cached tenant core (reconciler sweeps).
// Tenants not yet touched have no cached core; only cores that already served
// generations can have tasks in flight, which is exactly the cached set.
func (m *Manager) EachCore(fn func(core *agency.Core)) {
	m.mu.Lock()
	cached := make([]*Bundle, 0, len(m.bundles))
	for _, b := range m.bundles {
		cached = append(cached, b)
	}
	m.mu.Unlock()
	for _, b := range cached {
		fn(b.agencyCore)
	}
}

// Convenience resolver methods consumed by main when connecting modules.

// EventsFor returns the event handler for the request's tenant.
func (m *Manager) EventsFor(c *gin.Context) (*event.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Events, ok
}

// FilesFor returns the file handler for the request's tenant.
func (m *Manager) FilesFor(c *gin.Context) (*file.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Files, ok
}

// IngredientsFor returns the ingredient handler for the request's tenant.
func (m *Manager) IngredientsFor(c *gin.Context) (*ingredient.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Ingredients, ok
}

// AssignmentsFor returns the assignment handler for the request's tenant.
func (m *Manager) AssignmentsFor(c *gin.Context) (*assignment.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Assignments, ok
}

// PresetsFor returns the preset handler for the request's tenant.
func (m *Manager) PresetsFor(c *gin.Context) (*preset.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Presets, ok
}

// SkillsFor returns the skill handler for the request's tenant.
func (m *Manager) SkillsFor(c *gin.Context) (*skill.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Skills, ok
}

// PushFor returns the push handler for the request's tenant.
func (m *Manager) PushFor(c *gin.Context) (*push.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Push, ok
}

// CredentialsFor returns the credential handler for the request's tenant.
func (m *Manager) CredentialsFor(c *gin.Context) (*credential.Handler, bool) {
	b, ok := m.Resolve(c)
	return b.Credentials, ok
}

// AgencyCoreFor returns the agency core for the request's tenant.
func (m *Manager) AgencyCoreFor(c *gin.Context) (*agency.Core, bool) {
	b, ok := m.Resolve(c)
	return b.agencyCore, ok
}

// FlowsFor returns the agency flow store for the request's tenant.
func (m *Manager) FlowsFor(c *gin.Context) (toolkit.FlowsAPI, bool) {
	b, ok := m.Resolve(c)
	return b.Flows, ok
}

// build constructs the full service graph for one tenant over its own pool.
func (m *Manager) build(t *tenancy.Tenant) (*Bundle, error) {
	pool, err := m.reg.Pool(t.Slug)
	if err != nil {
		return nil, fmt.Errorf("tenant pool: %w", err)
	}

	// Stores + services (per-tenant).
	fileStore, err := file.NewStore(pool, m.cfg.UploadDir)
	if err != nil {
		return nil, fmt.Errorf("file store: %w", err)
	}
	fileSvc := file.NewService(fileStore, m.cfg.BaseURL, t.Slug, m.cfg.EncryptionKey, m.cfg.MaxUploadBytes, m.cfg.StorageAllowPrivateEndpoints)
	credStore := credential.NewStore(pool, m.cfg.EncryptionKey)
	eventSvc := event.NewService(event.NewStore(pool))
	ingredientSvc := ingredient.NewService(ingredient.NewStore(pool), m.cfg.BaseURL, t.Slug)
	assignmentSvc := assignment.NewService(assignment.NewStore(pool))
	presetSvc := preset.NewService(preset.NewStore(pool))
	skillSvc := skill.NewService(skill.NewStore(pool))
	pushSvc := push.NewService(push.NewStore(pool), m.cfg.PushVapidPublicKey, m.cfg.PushVapidPrivateKey, m.cfg.PushVapidSubject)

	// Agency orchestration core (per-tenant task tracking + logs).
	core := agency.NewCore(m.cfg, fileSvc, credStore, m.cfg.BaseURL, t.Slug)
	// Curaduría maestro + tenant también en generación directa por nombre:
	// el listado oculta, pero el veto real vive aquí.
	master := m.modelMaster
	core.SetModelPrefsCheck(func(modelName string) error {
		var masterMap map[string]model.MasterPref
		if master != nil {
			var err error
			masterMap, err = master.All()
			if err != nil {
				log.Printf("[runtime] master prefs for generation: %v", err)
				masterMap = map[string]model.MasterPref{}
			}
		}
		tenantMap := map[string]model.TenantPref{}
		tenStore := model.NewTenantPrefsStore(pool)
		if got, err := tenStore.All(); err != nil {
			log.Printf("[runtime] tenant prefs for generation: %v", err)
		} else {
			tenantMap = got
		}
		return model.CheckAllowed(modelName, masterMap, tenantMap)
	})
	core.SetStores(
		agency.NewAssetSyncStore(pool),
		agency.NewGenerationLogStore(pool),
		agency.NewServerCommunicationStore(pool),
		agency.NewGeneratedAssetStore(pool),
	)

	// Persist completed outputs into the piece's generation slot.
	core.SetGenerationSaver(func(pieceID string, number int, videoURL, localURL, taskID string) error {
		return eventSvc.SaveGenerationOutput(pieceID, number, videoURL, localURL, taskID)
	})
	core.SetPushNotifier(pushSvc)
	// Generated media follows the same rule as uploads: the tenant's bucket
	// when configured, local disk otherwise.
	core.SetOutputPublisher(fileSvc)

	// Generators (video ×3, image ×3, text ×1). Audio has no generator yet.
	core.RegisterGenerator(agencyvideo.NewSeedanceGenerator())
	core.RegisterGenerator(agencyvideo.NewSeedanceGalleryGenerator())
	core.RegisterGenerator(agencyvideo.NewSeedance25Generator())
	core.RegisterGenerator(agencyimage.NewSeedreamGenerator())
	core.RegisterGenerator(agencyimage.NewGeminiNanoGenerator())
	core.RegisterGenerator(agencyimage.NewGeminiProGenerator())
	core.RegisterGenerator(agencytext.NewClaudeTextGenerator())
	// Local LLM servers the tenant runs (Ollama, LM Studio): names are
	// "<provider>/<model id>", discovered live from /v1/models. Registered
	// before the downloaded generator, which claims any unknown name.
	core.RegisterGenerator(agencytext.NewOpenAITextGenerator())
	// Hosted inference clouds the tenant pays for itself: the OpenAI-compatible
	// images route (Together, DeepInfra, OpenRouter) and RunPod serverless
	// endpoints. Both match only "<provider>/…" names, so the curated catalog
	// keeps its own generators.
	core.RegisterGenerator(agencyimage.NewOpenAIImageGenerator())
	core.RegisterGenerator(agencyimage.NewRunPodGenerator())
	// Image Higgsfield FIRST: its Match is narrow (soul/ideogram/recraft) while
	// the video one claims any "higgsfield-*", so registering video first would
	// shadow the image models (Soul 2 et al.) and route them to the video
	// generator.
	core.RegisterGenerator(agencyimage.NewHiggsfieldImageGenerator())
	core.RegisterGenerator(agencyvideo.NewHiggsfieldGenerator())
	// OpenRouter dedicated video API (Veo, Kling, Wan, Seedance, Hailuo…):
	// async submit/poll/download. Registered after Higgsfield so the
	// curated catalog keeps precedence on overlapping names (there is none:
	// hosted names are provider-namespaced).
	core.RegisterGenerator(agencyvideo.NewOpenRouterVideoGenerator())

	// Downloaded-model generator — registered LAST on purpose: it claims any
	// model name outside the API catalog, so it must not shadow API
	// generators. Two dispatch paths, chosen by configuration:
	//   - BM_WORKER_ADDR set → direct gRPC (local / rented VM with tunnel).
	//   - bridge store wired (SetWorkerBridge) → outbound queue: the user's
	//     GPU pulls jobs over HTTP; state lives in worker_jobs (survives
	//     restarts on both sides).
	dlGen := agencyvideo.NewDownloadedGenerator(
		worker.NewClient(), m.cfg.WorkerAddr, m.cfg.OutputsDir)
	// Bridge siempre activo cuando hay store: cada usuario puede emparejar
	// su propia GPU (multi-usuario) y generar aunque haya un worker
	// central configurado. El generator prefiere el central cuando está
	// vivo y cae al bridge cuando no lo está o no tiene el modelo.
	if m.workerBridgeStore != nil {
		dlGen.SetBridge(m.workerBridgeStore, t.ID)
		// Los snapshots de las GPUs emparejadas del tenant son fuente viva
		// de modelos "downloaded" junto al worker central: el core los
		// resuelve para generar (modalidad incluida) en vez de responder
		// "model not found" a todo modelo que sólo vive en la máquina del
		// usuario.
		core.SetBridgeModels(bridgeModelResolver(m.workerBridgeStore, t.ID))
	}
	core.RegisterGenerator(dlGen)

	// Cost calculators. Seedance25 must precede Seedance: the latter's
	// substring match ("dreamina-seedance") also covers the 2.5 model.
	core.RegisterCalculator(calculators.NewSeedance25Calculator())
	core.RegisterCalculator(calculators.NewSeedanceCalculator())
	core.RegisterCalculator(calculators.NewSeedreamCalculator())
	core.RegisterCalculator(calculators.NewGeminiCalculator())
	core.RegisterCalculator(calculators.NewDefaultCalculator())

	return &Bundle{
		Tenant:      t,
		Events:      event.NewHandler(eventSvc),
		Files:       file.NewHandler(fileSvc),
		Ingredients: ingredient.NewHandler(ingredientSvc),
		Assignments: assignment.NewHandler(assignmentSvc),
		Presets:     preset.NewHandler(presetSvc),
		Skills:      skill.NewHandler(skillSvc),
		Push:        push.NewHandler(pushSvc),
		Credentials: credential.NewHandler(credStore),
		Agency:      agency.NewHandler(core),
		Flows:       toolkit.NewPGFlows(pool),
		agencyCore:  core,
	}, nil
}
