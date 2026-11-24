package workerbridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ─── Fakes ─────────────────────────────────────────────────────

// fakeHealthSource reproduce el estado del registry sin Postgres.
type fakeHealthSource struct {
	tenants  []int64
	workers  map[int64][]WorkerHealth
	admins   []int64
	episodes map[string]string // "worker|kind" → clave del episodio abierto
	expired  int
}

func newFakeHealthSource() *fakeHealthSource {
	return &fakeHealthSource{
		workers:  map[int64][]WorkerHealth{},
		episodes: map[string]string{},
	}
}

func (f *fakeHealthSource) ActiveTenantIDs(context.Context) ([]int64, error) {
	return f.tenants, nil
}

func (f *fakeHealthSource) AlertRecipients(context.Context, int64) ([]int64, error) {
	return f.admins, nil
}

func (f *fakeHealthSource) AlertCandidates(_ context.Context, tenantID int64) ([]WorkerHealth, error) {
	return f.workers[tenantID], nil
}

func (f *fakeHealthSource) ExpireUpdates(context.Context, int64, time.Duration) (int64, error) {
	f.expired++
	return 0, nil
}

func (f *fakeHealthSource) OpenAlertEpisode(_ context.Context, workerID, kind, key string) (bool, error) {
	id := workerID + "|" + kind
	if current, ok := f.episodes[id]; ok && current == key {
		return false, nil
	}
	f.episodes[id] = key
	return true, nil
}

func (f *fakeHealthSource) CloseAlertEpisode(_ context.Context, workerID, kind string) error {
	delete(f.episodes, workerID+"|"+kind)
	return nil
}

// fakeNotifier registra lo que se manda al tenant.
type fakeNotifier struct {
	channels []string
	admins   []string
}

func (f *fakeNotifier) NotifyTenantChannels(_ int64, title, body string, _ map[string]string) {
	f.channels = append(f.channels, title+" | "+body)
}

func (f *fakeNotifier) NotifyTenantAdmin(_ int64, _ int64, ntype, title, _ string) {
	f.admins = append(f.admins, ntype+" "+title)
}

func healthMonitorAt(src healthSource, n HealthNotifier, now time.Time) *HealthMonitor {
	h := NewHealthMonitor(src, n)
	h.now = func() time.Time { return now }
	return h
}

// ─── Tests ─────────────────────────────────────────────────────

// Una GPU caída avisa una sola vez: las pasadas siguientes del monitor no
// repiten el mismo aviso (ni a los canales ni a los admins).
func TestHealthMonitorAlertsOncePerEpisode(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := newFakeHealthSource()
	src.tenants = []int64{1}
	src.admins = []int64{7}
	src.workers[1] = []WorkerHealth{{
		ID: "w-1", Name: "PC-Casa", GPUName: "RTX 3060",
		LastHeartbeat: now.Add(-20 * time.Minute),
	}}
	notif := &fakeNotifier{}
	h := healthMonitorAt(src, notif, now)

	h.evaluate(context.Background())
	h.evaluate(context.Background())

	if len(notif.channels) != 1 {
		t.Fatalf("canales = %v, want una sola alerta", notif.channels)
	}
	if !strings.Contains(notif.channels[0], "PC-Casa") || !strings.Contains(notif.channels[0], "20 min") {
		t.Fatalf("aviso = %q, want la máquina y hace cuánto no late", notif.channels[0])
	}
	if len(notif.admins) != 1 || !strings.HasPrefix(notif.admins[0], "gpu.offline") {
		t.Fatalf("admins = %v, want un aviso gpu.offline", notif.admins)
	}
	// El watchdog del reinicio corre en cada pasada, aunque nadie abra el panel.
	if src.expired != 2 {
		t.Fatalf("expire updates = %d, want una por pasada", src.expired)
	}
}

// Cuando la máquina vuelve, el episodio se cierra: la próxima caída avisa otra
// vez en vez de quedar silenciada para siempre.
func TestHealthMonitorReAlertsAfterRecovery(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := newFakeHealthSource()
	src.tenants = []int64{1}
	notif := &fakeNotifier{}
	h := healthMonitorAt(src, notif, now)

	src.workers[1] = []WorkerHealth{{ID: "w-1", Name: "PC-Casa",
		LastHeartbeat: now.Add(-10 * time.Minute)}}
	h.evaluate(context.Background())

	// Volvió: el heartbeat es reciente y no hay nada más roto.
	src.workers[1] = []WorkerHealth{{ID: "w-1", Name: "PC-Casa",
		LastHeartbeat: now.Add(-10 * time.Second),
		DiskFreeMB:    50 * 1024}}
	h.evaluate(context.Background())

	src.workers[1] = []WorkerHealth{{ID: "w-1", Name: "PC-Casa",
		LastHeartbeat: now.Add(-8 * time.Minute)}}
	h.evaluate(context.Background())

	if len(notif.channels) != 2 {
		t.Fatalf("canales = %v, want una alerta por cada caída", notif.channels)
	}
}

// Dos actualizaciones fallidas distintas son dos episodios: la clave es el id
// del pedido, así el segundo intento vuelve a avisar.
func TestHealthMonitorUpdateFailurePerRequest(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := newFakeHealthSource()
	src.tenants = []int64{1}
	src.admins = []int64{7, 9}
	notif := &fakeNotifier{}
	h := healthMonitorAt(src, notif, now)

	failed := func(id string) []WorkerHealth {
		return []WorkerHealth{{
			ID: "w-1", Name: "PC-Casa", GPUName: "RTX 3060",
			LastHeartbeat: now.Add(-10 * time.Second),
			UpdateFailure: &UpdateFailure{ID: id, Error: "descarga falló: timeout",
				At: now},
		}}
	}

	src.workers[1] = failed("u-1")
	h.evaluate(context.Background())
	h.evaluate(context.Background()) // mismo pedido: no repite

	src.workers[1] = failed("u-2")
	h.evaluate(context.Background())

	if len(notif.channels) != 2 {
		t.Fatalf("canales = %v, want un aviso por actualización fallida", notif.channels)
	}
	if !strings.Contains(notif.channels[0], "descarga falló: timeout") {
		t.Fatalf("aviso = %q, want el motivo que reportó el worker", notif.channels[0])
	}
	// Dos admins: cada aviso les llega a los dos.
	if len(notif.admins) != 4 {
		t.Fatalf("admins = %v, want 2 avisos por episodio para 2 admins", notif.admins)
	}
}

// Con la GPU caída, el disco y los jobs no avisan: son datos del último
// heartbeat y sólo agregarían ruido encima del aviso de conexión.
func TestHealthMonitorSkipsStaleDataWhenOffline(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := newFakeHealthSource()
	src.tenants = []int64{1}
	src.workers[1] = []WorkerHealth{{
		ID: "w-1", Name: "PC-Casa", LastHeartbeat: now.Add(-2 * time.Hour),
		DiskFreeMB: 800, FailedStreak: true, LastJobError: "CUDA OOM",
	}}
	notif := &fakeNotifier{}
	h := healthMonitorAt(src, notif, now)

	h.evaluate(context.Background())

	if len(notif.channels) != 1 || !strings.Contains(notif.channels[0], "sin conexión") {
		t.Fatalf("canales = %v, want sólo el aviso de conexión", notif.channels)
	}
}

// Disco bajo y racha de jobs fallidos avisan con el detalle que necesita el
// admin para actuar, y también una sola vez por episodio.
func TestHealthMonitorAlertsLowDiskAndFailingJobs(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	src := newFakeHealthSource()
	src.tenants = []int64{1}
	src.admins = []int64{7}
	src.workers[1] = []WorkerHealth{{
		ID: "w-1", Name: "PC-Casa", LastHeartbeat: now.Add(-5 * time.Second),
		DiskFreeMB: 2 * 1024, FailedStreak: true, LastJobError: "CUDA out of memory",
	}}
	notif := &fakeNotifier{}
	h := healthMonitorAt(src, notif, now)

	h.evaluate(context.Background())
	h.evaluate(context.Background())

	if len(notif.channels) != 2 {
		t.Fatalf("canales = %v, want disco + jobs", notif.channels)
	}
	joined := strings.Join(notif.channels, "\n")
	if !strings.Contains(joined, "2.0 GB") {
		t.Fatalf("aviso de disco = %q, want el espacio libre", joined)
	}
	if !strings.Contains(joined, "CUDA out of memory") {
		t.Fatalf("aviso de jobs = %q, want el último error", joined)
	}
	if len(notif.admins) != 2 {
		t.Fatalf("admins = %v, want un aviso por cada problema", notif.admins)
	}
}
