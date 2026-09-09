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
	"fmt"
	"log"
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
	"synapta/internal/modules/preset"
	"synapta/internal/modules/push"
	"synapta/internal/modules/skill"
	"synapta/internal/tenancy"
)

// Manager builds and caches per-tenant bundles.
type Manager struct {
	cfg *config.Config
	reg *tenancy.Registry

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

	agencyCore *agency.Core // for the reconciler and modality resolvers
}

// NewManager creates a bundle manager over the tenancy registry.
func NewManager(cfg *config.Config, reg *tenancy.Registry) *Manager {
	return &Manager{cfg: cfg, reg: reg, bundles: map[string]*Bundle{}}
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
	fileSvc := file.NewService(fileStore, m.cfg.BaseURL)
	credStore := credential.NewStore(pool, m.cfg.EncryptionKey)
	eventSvc := event.NewService(event.NewStore(pool))
	ingredientSvc := ingredient.NewService(ingredient.NewStore(pool), m.cfg.BaseURL)
	assignmentSvc := assignment.NewService(assignment.NewStore(pool))
	presetSvc := preset.NewService(preset.NewStore(pool))
	skillSvc := skill.NewService(skill.NewStore(pool))
	pushSvc := push.NewService(push.NewStore(pool), m.cfg.PushVapidPublicKey, m.cfg.PushVapidPrivateKey, m.cfg.PushVapidSubject)

	// Agency orchestration core (per-tenant task tracking + logs).
	core := agency.NewCore(m.cfg, fileSvc, credStore, m.cfg.BaseURL, t.Slug)
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

	// Generators (video ×3, image ×3, text ×1). Audio has no generator yet.
	core.RegisterGenerator(agencyvideo.NewSeedanceGenerator())
	core.RegisterGenerator(agencyvideo.NewSeedanceGalleryGenerator())
	core.RegisterGenerator(agencyvideo.NewSeedance25Generator())
	core.RegisterGenerator(agencyimage.NewSeedreamGenerator())
	core.RegisterGenerator(agencyimage.NewGeminiNanoGenerator())
	core.RegisterGenerator(agencyimage.NewGeminiProGenerator())
	core.RegisterGenerator(agencytext.NewClaudeTextGenerator())

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
		agencyCore:  core,
	}, nil
}
