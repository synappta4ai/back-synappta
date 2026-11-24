package workerbridge

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// ─── Alertas de salud de las GPUs emparejadas ──────────────────
//
// Una GPU que deja de latir no se nota hasta que alguien abre el panel: el
// trabajo de todo el equipo se encola contra una máquina que no está. Este
// monitor revisa el registry y avisa por los canales del tenant y a sus admins.
//
// Una sola alerta por episodio (worker_alert_state): mientras la condición
// siga, no se repite; cuando se cura, el episodio se cierra y el próximo fallo
// vuelve a avisar. Sin eso, cada pasada del monitor spammea los mismos avisos.

// Umbrales. Son más holgados que los del panel a propósito: una alerta que
// llega tarde molesta menos que una que dispara un reinicio normal del worker
// (~1-2 min) o un pico de disco que se limpia solo.
const (
	// offlineAlertAfter: sin heartbeat por este lapso = la máquina se cayó. El
	// worker late cada 20 s.
	offlineAlertAfter = 5 * time.Minute
	// lowDiskAlertMB: mismo umbral que el aviso del panel (5 GB libres).
	lowDiskAlertMB = 5 * 1024
	// failedJobStreak: jobs terminados consecutivos que tienen que fallar para
	// avisar de que esa GPU no está produciendo.
	failedJobStreak = 3
)

// Kinds de alerta (también el `kind` de worker_alert_state).
const (
	alertOffline      = "offline"
	alertLowDisk      = "low_disk"
	alertUpdateFailed = "update_failed"
	alertJobsFailing  = "jobs_failing"
)

// allAlertKinds lista los kinds para poder cerrar los que ya no aplican.
var allAlertKinds = []string{alertOffline, alertLowDisk, alertUpdateFailed, alertJobsFailing}

// HealthNotifier entrega un aviso a la gente del tenant. Lo implementa el
// runtime, que tiene los servicios por-tenant (canales externos, campanita y
// push): este módulo vive sobre el schema de sistema y no los conoce.
type HealthNotifier interface {
	// NotifyTenantChannels postea a los canales del tenant (Slack, Telegram…).
	NotifyTenantChannels(tenantID int64, title, body string, data map[string]string)
	// NotifyTenantAdmin avisa a un admin por la campanita y el push del navegador.
	NotifyTenantAdmin(tenantID, userID int64, ntype, title, body string)
}

// WorkerHealth es el estado de una GPU que el monitor evalúa.
type WorkerHealth struct {
	ID            string
	Name          string
	GPUName       string
	Version       string
	LastHeartbeat time.Time
	DiskFreeMB    int64
	// UpdateFailure es el último intento de actualización que falló: mientras
	// exista, la GPU queda con el código viejo (o a medio actualizar).
	UpdateFailure *UpdateFailure
	// FailedStreak: los últimos `failedJobStreak` jobs terminados fallaron.
	FailedStreak bool
	LastJobError string
	// Paused: la GPU está en mantenimiento a propósito (no reclama trabajos).
	Paused bool
}

// healthSource es lo que el monitor necesita del store. Interface propia para
// testear la lógica de episodios sin Postgres.
type healthSource interface {
	ActiveTenantIDs(ctx context.Context) ([]int64, error)
	AlertRecipients(ctx context.Context, tenantID int64) ([]int64, error)
	AlertCandidates(ctx context.Context, tenantID int64) ([]WorkerHealth, error)
	ExpireUpdates(ctx context.Context, tenantID int64, timeout time.Duration) (int64, error)
	OpenAlertEpisode(ctx context.Context, workerID, kind, key string) (bool, error)
	CloseAlertEpisode(ctx context.Context, workerID, kind string) error
}

// alertText es un aviso listo para mandar: el kind, la clave del episodio (para
// no repetirlo) y el texto que ve el admin.
type alertText struct {
	kind  string
	key   string
	title string
	body  string
}

// HealthMonitor revisa periódicamente las GPUs y avisa una vez por episodio.
type HealthMonitor struct {
	src      healthSource
	notifier HealthNotifier
	now      func() time.Time
}

// NewHealthMonitor construye el monitor sobre el store y el notifier del runtime.
func NewHealthMonitor(src healthSource, notifier HealthNotifier) *HealthMonitor {
	return &HealthMonitor{src: src, notifier: notifier, now: time.Now}
}

// Run evalúa hasta que se cierre `stop` (mismo patrón que los reconcilers del
// main). La primera pasada es inmediata, así un despliegue con una GPU caída
// avisa sin esperar al primer tick.
func (h *HealthMonitor) Run(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ctx := context.Background()
	for {
		h.evaluate(ctx)
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

// evaluate hace una pasada sobre todos los tenants activos.
func (h *HealthMonitor) evaluate(ctx context.Context) {
	tenants, err := h.src.ActiveTenantIDs(ctx)
	if err != nil {
		log.Printf("[gpuhealth] tenants activos: %v", err)
		return
	}
	for _, tenantID := range tenants {
		if err := h.evaluateTenant(ctx, tenantID); err != nil {
			log.Printf("[gpuhealth] tenant %d: %v", tenantID, err)
		}
	}
}

func (h *HealthMonitor) evaluateTenant(ctx context.Context, tenantID int64) error {
	// El watchdog del reinicio corre también acá: si nadie abre el panel, una
	// actualización que no volvió igual tiene que reportarse y avisarse.
	if _, err := h.src.ExpireUpdates(ctx, tenantID, updateRestartTimeout); err != nil {
		log.Printf("[gpuhealth] tenant %d, expirar actualizaciones: %v", tenantID, err)
	}
	workers, err := h.src.AlertCandidates(ctx, tenantID)
	if err != nil {
		return err
	}
	recipients, rerr := h.src.AlertRecipients(ctx, tenantID)
	if rerr != nil {
		// Sin admins todavía se avisa por los canales del tenant.
		log.Printf("[gpuhealth] tenant %d, admins: %v", tenantID, rerr)
	}
	for _, w := range workers {
		h.evaluateWorker(ctx, tenantID, w, recipients)
	}
	return nil
}

func (h *HealthMonitor) evaluateWorker(ctx context.Context, tenantID int64, w WorkerHealth, recipients []int64) {
	active := h.alerts(w)
	open := map[string]bool{}
	for _, a := range active {
		open[a.kind] = true
		isNew, err := h.src.OpenAlertEpisode(ctx, w.ID, a.kind, a.key)
		if err != nil {
			log.Printf("[gpuhealth] episodio %s/%s: %v", shortID(w.ID), a.kind, err)
			continue
		}
		if !isNew {
			continue // mismo episodio: ya se avisó
		}
		h.notify(tenantID, recipients, a)
	}
	// Lo que se curó cierra el episodio: el próximo fallo vuelve a avisar.
	for _, kind := range allAlertKinds {
		if open[kind] {
			continue
		}
		if err := h.src.CloseAlertEpisode(ctx, w.ID, kind); err != nil {
			log.Printf("[gpuhealth] cerrar %s/%s: %v", shortID(w.ID), kind, err)
		}
	}
}

// alerts traduce el estado de una GPU a los avisos que corresponden.
func (h *HealthMonitor) alerts(w WorkerHealth) []alertText {
	label := workerLabel(w)
	offline := w.LastHeartbeat.IsZero() || h.now().Sub(w.LastHeartbeat) > offlineAlertAfter
	var out []alertText

	// El fallo de actualización se avisa aunque la máquina esté caída o en
	// pausa: es justamente el caso "se reinició y no volvió".
	if w.UpdateFailure != nil {
		reason := strings.TrimSpace(w.UpdateFailure.Error)
		if reason == "" {
			reason = "el worker no reportó el motivo"
		}
		out = append(out, alertText{
			kind:  alertUpdateFailed,
			key:   w.UpdateFailure.ID,
			title: "La actualización falló en " + label,
			body:  fmt.Sprintf("%s sigue con la versión anterior. Motivo: %s", label, reason),
		})
	}

	// En pausa la máquina puede estar apagada a propósito (mantenimiento): no
	// tiene sentido avisar que no late, ni de su disco ni de trabajos que por
	// definición no está tomando.
	if w.Paused {
		return out
	}

	if offline {
		body := fmt.Sprintf("%s se emparejó pero nunca respondió.", label)
		if !w.LastHeartbeat.IsZero() {
			body = fmt.Sprintf("%s dejó de responder hace %s; los trabajos que le toquen van a esperar a que vuelva.",
				label, humanDuration(h.now().Sub(w.LastHeartbeat)))
		}
		out = append(out, alertText{
			kind:  alertOffline,
			title: "GPU sin conexión: " + label,
			body:  body,
		})
	}

	// Disco y jobs son datos del último heartbeat: con la máquina caída están
	// viejos y sólo agregarían ruido.
	if offline {
		return out
	}
	if w.DiskFreeMB > 0 && w.DiskFreeMB < lowDiskAlertMB {
		out = append(out, alertText{
			kind:  alertLowDisk,
			title: "Poco disco en " + label,
			body: fmt.Sprintf("Quedan %s libres donde viven los pesos: no entra otro modelo hasta borrar algo.",
				humanSize(w.DiskFreeMB)),
		})
	}
	if w.FailedStreak {
		body := fmt.Sprintf("Los últimos %d trabajos fallaron.", failedJobStreak)
		if errMsg := strings.TrimSpace(w.LastJobError); errMsg != "" {
			body = fmt.Sprintf("Los últimos %d trabajos fallaron. Último error: %s", failedJobStreak, errMsg)
		}
		out = append(out, alertText{
			kind:  alertJobsFailing,
			title: "Trabajos fallando en " + label,
			body:  body,
		})
	}
	return out
}

// notify manda el aviso a los canales del tenant y a cada admin.
func (h *HealthMonitor) notify(tenantID int64, recipients []int64, a alertText) {
	if h.notifier == nil {
		return
	}
	log.Printf("[gpuhealth] alerta %s: %s", a.kind, a.title)
	h.notifier.NotifyTenantChannels(tenantID, a.title, a.body, map[string]string{"type": "gpu." + a.kind})
	for _, userID := range recipients {
		h.notifier.NotifyTenantAdmin(tenantID, userID, "gpu."+a.kind, a.title, a.body)
	}
}

// workerLabel es cómo se nombra la GPU en el aviso: nombre de la máquina + placa.
func workerLabel(w WorkerHealth) string {
	name := strings.TrimSpace(w.Name)
	if name == "" {
		name = "GPU " + shortID(w.ID)
	}
	if gpu := strings.TrimSpace(w.GPUName); gpu != "" {
		return fmt.Sprintf("«%s» (%s)", name, gpu)
	}
	return fmt.Sprintf("«%s»", name)
}

// humanDuration redondea a minutos u horas: el aviso no necesita segundos.
func humanDuration(d time.Duration) string {
	if d < time.Hour {
		minutes := int(d.Minutes())
		if minutes < 1 {
			minutes = 1
		}
		return fmt.Sprintf("%d min", minutes)
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}

// humanSize pasa MB a GB con un decimal (0 en vez de "0.0 GB").
func humanSize(mb int64) string {
	if mb <= 0 {
		return "0 GB"
	}
	return fmt.Sprintf("%.1f GB", float64(mb)/1024)
}
