// Reconciler: recovers non-final generation tasks after a restart.
//
// dcs-back only recovered a task's state when a client polled its status.
// Synapta adds a startup loop that re-polls every non-final task found in
// generation_logs, so a deploy or crash does not orphan running generations
// (video tasks can run for minutes).
package agency

import (
	"log"
	"time"

	"synapta/config"
)

// CoreSweeper runs fn over every per-tenant core (satisfied by the runtime
// manager; defined here to avoid an import cycle).
type CoreSweeper func(fn func(core *Core))

// ReconcileLoop sweeps every tenant core every 2 minutes (with an initial
// sweep shortly after boot), re-polling non-final generation logs through the
// normal status path until they terminate. Runs until stop is closed.
func ReconcileLoop(sweep CoreSweeper, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()

		first := time.After(10 * time.Second)
		for {
			select {
			case <-stop:
				return
			case <-first:
				first = nil
				sweep(func(core *Core) { core.reconcileOnce() })
			case <-ticker.C:
				sweep(func(core *Core) { core.reconcileOnce() })
			}
		}
	}()
}

// ReconcileOnce polls every non-final task once. Exported so the process-level
// reconciler in main can sweep every per-tenant core.
func (s *Core) ReconcileOnce() { s.reconcileOnce() }

// reconcileOnce polls every non-final task once.
func (s *Core) reconcileOnce() {
	if s.logStore == nil {
		return
	}
	logs, err := s.logStore.ListNonFinalTaskIDs(25)
	if err != nil {
		log.Printf("[reconciler] list non-final tasks: %v", err)
		return
	}
	for _, entry := range logs {
		if entry.TaskID == "" || entry.TaskID == "<no-task>" {
			continue
		}
		// Skip tasks already tracked in memory (their status is fresh).
		s.mu.RLock()
		_, tracked := s.tasks[entry.TaskID]
		s.mu.RUnlock()
		if tracked {
			continue
		}

		result, err := s.GetStatus(entry.TaskID)
		if err != nil {
			log.Printf("[reconciler] poll task %s: %v", entry.TaskID, err)
			continue
		}
		if result.Status == config.STATUS_SUCCESS || result.Status == config.STATUS_FAILED {
			log.Printf("[reconciler] task %s recovered with status %s", entry.TaskID, result.Status)
		}
	}
}
