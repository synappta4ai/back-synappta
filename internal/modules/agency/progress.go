// Video generation metadata + progress estimation.
//
// Providers rarely expose a progress field, so completion percent is estimated
// from the task's elapsed wall-clock time against an expected duration per
// model, blended with any provider-provided progress. The estimate is
// persisted (monotonically) into generation_logs.progress on every poll so it
// survives restarts and can be served to the front.
package agency

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"synapta/config"
	"synapta/internal/utils"
)

// expectedVideoDuration is the typical wall-clock time a video task takes for
// a 10s clip, used as the scale for the progress ramp.
func expectedVideoDuration(model string) time.Duration {
	m := strings.ToLower(model)
	base := 150 * time.Second // Seedance 2.0 default
	switch {
	case strings.Contains(m, "seedance-2-5"):
		base = 240 * time.Second // 2.5 models are heavier
	case strings.Contains(m, "seedance"):
		base = 150 * time.Second
	default:
		base = 120 * time.Second
	}
	return base
}

// elapsedVideoPercent maps elapsed time onto a conservative ramp: fast early
// gains, decelerating as it approaches 90 (100 is only confirmed by the
// provider's terminal status).
func elapsedVideoPercent(elapsed, expected time.Duration) int {
	if elapsed <= 0 || expected <= 0 {
		return 0
	}
	ratio := elapsed.Seconds() / expected.Seconds()
	pct := 5 + 85*(1-math.Exp(-1.6*ratio))
	if pct > 90 {
		pct = 90
	}
	if pct < 1 {
		pct = 1
	}
	return int(pct)
}

// numeric helper — provider JSON decodes numbers as float64.
func numeric(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func intFromAny(v interface{}) int {
	if n, ok := numeric(v); ok {
		return int(n)
	}
	return 0
}

func int64FromAny(v interface{}) int64 {
	if n, ok := numeric(v); ok {
		return int64(n)
	}
	return 0
}

// providerProgressPct extracts an explicit progress percent from the provider
// response when it exposes one (keys: progress, percentage, task_progress).
func providerProgressPct(raw interface{}) int {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return 0
	}
	for _, key := range []string{"progress", "percentage", "task_progress"} {
		if n, ok := numeric(m[key]); ok {
			if n > 0 && n <= 1 {
				n *= 100 // provider gave a 0-1 fraction
			}
			if n >= 0 && n <= 100 {
				return int(n)
			}
		}
	}
	return 0
}

// ExtractVideoMetadata pulls the important fields out of a provider status
// response (BytePlus Ark shape; tolerant to missing fields).
func ExtractVideoMetadata(raw interface{}) VideoMetadata {
	md := VideoMetadata{}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return md
	}
	md.Duration = intFromAny(m["duration"])
	md.FPS = intFromAny(m["framespersecond"])
	md.Resolution, _ = m["resolution"].(string)
	md.Ratio, _ = m["ratio"].(string)
	md.Seed = int64FromAny(m["seed"])
	if u, ok := m["usage"].(map[string]interface{}); ok {
		md.UsageTokens = int64FromAny(u["total_tokens"])
		md.UsageCompletionTokens = int64FromAny(u["completion_tokens"])
	}
	return md
}

// trackVideoMetadata samples a poll response into generation_logs: provider
// metadata (usage tokens, duration, resolution, seed, fps) plus the progress
// estimate. Progress writes use GREATEST() so it never regresses; success pins
// it to 100.
func (s *Core) trackVideoMetadata(taskID string, createdAt time.Time, resourceType, modelName string, result *GeneratorResult) {
	if s.logStore == nil || result == nil {
		return
	}
	if resourceType != "" && resourceType != config.ModalityVideo {
		return
	}

	md := ExtractVideoMetadata(result.Raw)
	md.Progress = providerProgressPct(result.Raw)

	switch result.Status {
	case config.STATUS_SUCCESS:
		md.Progress = 100
	case config.STATUS_FAILED, config.STATUS_CANCELLED:
		// terminal failure: keep whatever progress was reached
	default:
		if e := elapsedVideoPercent(time.Since(createdAt), expectedVideoDuration(modelName)); e > md.Progress {
			md.Progress = e
		}
	}

	if err := s.logStore.UpdateMetadataByTaskID(taskID, md); err != nil {
		log.Printf("[metadata] persist failed for task %s: %v", taskID, err)
	}
}

// ensureLocalVideo downloads the provider's video output into the server's
// outputs directory (once) so the front can fetch it from our own domain
// instead of the expiring signed provider URL.
func (s *Core) ensureLocalVideo(taskID string, result *GeneratorResult) {
	if result == nil || len(result.Outputs) == 0 {
		return
	}
	out := &result.Outputs[0]
	if out.LocalURL != "" || out.URL == "" {
		return
	}
	// Task-stable filename: re-polls of the same task reuse the same file.
	localName := fmt.Sprintf("video_%s.mp4", taskID)
	localURL, err := utils.SaveURLOutput(out.URL, localName)
	if err != nil {
		log.Printf("[outputs] download failed for task %s: %v", taskID, err)
		return
	}
	out.LocalURL = localURL
	log.Printf("[outputs] video stored locally for task %s: %s", taskID, localURL)
}

// absoluteOutputURL converts a stored relative output path into an absolute,
// server-owned URL the front can play directly.
func (s *Core) absoluteOutputURL(localURL string) string {
	if localURL == "" {
		return ""
	}
	if strings.HasPrefix(localURL, "http://") || strings.HasPrefix(localURL, "https://") {
		return localURL
	}
	if !strings.HasPrefix(localURL, "/") {
		localURL = "/" + localURL
	}
	return s.baseURL + localURL
}
