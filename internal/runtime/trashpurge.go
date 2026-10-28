package runtime

import (
	"log"
	"time"

	"synapta/config"
	"synapta/internal/modules/agency"
	"synapta/internal/modules/file"
	"synapta/internal/tenancy"
)

// TrashPurgeLoop permanently removes trash older than its retention (30
// days) in every active tenant: trashed generation_logs (rows + local
// output files) and trashed library files (disk/bucket object + row).
// First sweep runs shortly after boot (catches up after downtime), then
// nightly. Runs until stop is closed.
func TrashPurgeLoop(reg *tenancy.Registry, cfg *config.Config, stop <-chan struct{}) {
	go func() {
		sweep := func() {
			tenants, err := reg.ListActiveTenants()
			if err != nil {
				log.Printf("[trash-purge] list tenants: %v", err)
				return
			}
			for i := range tenants {
				purgeTenantTrash(reg, cfg, tenants[i].Slug)
			}
		}

		first := time.After(5 * time.Minute)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-first:
				first = nil
				sweep()
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

// purgeTenantTrash purges one tenant's expired trash (generations + files).
func purgeTenantTrash(reg *tenancy.Registry, cfg *config.Config, slug string) {
	pool, err := reg.Pool(slug)
	if err != nil {
		log.Printf("[trash-purge] tenant %q pool: %v", slug, err)
		return
	}
	// Generations trash: bare core (log store + outputs dir is all the
	// purge needs; no generators, no notifiers).
	core := agency.NewCore(cfg, nil, nil, cfg.BaseURL, slug)
	core.SetStores(nil, agency.NewGenerationLogStore(pool), nil, nil)
	if n, err := core.PurgeTrash(); err != nil {
		log.Printf("[trash-purge] tenant %q generations: %v", slug, err)
	} else if n > 0 {
		log.Printf("[trash-purge] tenant %q: %d trashed generations removed", slug, n)
	}
	// Library files trash.
	fs, err := file.NewStore(pool, cfg.UploadDir)
	if err != nil {
		log.Printf("[trash-purge] tenant %q file store: %v", slug, err)
		return
	}
	svc := file.NewService(fs, cfg.BaseURL, slug, cfg.EncryptionKey, cfg.MaxUploadBytes, cfg.StorageAllowPrivateEndpoints)
	if n, err := svc.PurgeExpiredTrash(); err != nil {
		log.Printf("[trash-purge] tenant %q files: %v", slug, err)
	} else if n > 0 {
		log.Printf("[trash-purge] tenant %q: %d trashed files removed", slug, n)
	}
}
