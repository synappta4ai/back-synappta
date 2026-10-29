package infoproduct

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// The DCS-ANALYZE transport delivers video at 1 fps, so a 15 s ad with 18 cuts
// comes back with 5-7 shots: the model cannot see boundaries between sampled
// frames. ffmpeg already measured every cut, so the timeline is decided by that
// measurement and the model only fills the per-shot forensics. Every derived
// count/aggregate is recomputed so the document stays self-consistent.
//
// Model data is never rewritten silently: the reconciled timeline is marked in
// quality (timeline_source/reconciled/model_shots) and the model's own shot
// list is kept in quality.model_shots_list.
const (
	reconcileBoundaryTolerance = 0.05
	reconcileMinShot           = 0.001
	reconcileMicroShotSeconds  = 0.30
)

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func deepCopy(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// reconcileTimeline splits the model shot list at every ffmpeg-measured cut
// (keeping the model's own extra cuts) so the timeline covers the whole video.
// It reports whether the document had to change.
func reconcileTimeline(raw json.RawMessage, measuredCuts []float64) (json.RawMessage, bool) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, false
	}
	video := asMap(root["video"])
	legacy := asMap(root["legacy_ui"])
	if root["shots"] == nil {
		return raw, false
	}
	originalShots := deepCopy(root["shots"])
	duration, _ := asFloat(video["duration_s"])
	if duration <= 0 {
		duration, _ = asFloat(legacy["duracion_s"])
	}
	if duration <= 0 {
		return raw, false
	}

	type span struct {
		start, end float64
		shot       map[string]any
		toma       map[string]any
	}
	tomas := asSlice(legacy["tomas"])
	spans := make([]span, 0, len(asSlice(root["shots"])))
	for i, entry := range asSlice(root["shots"]) {
		shot := asMap(entry)
		if shot == nil {
			continue
		}
		start, _ := asFloat(shot["start_s"])
		end, _ := asFloat(shot["end_s"])
		if end <= start {
			continue
		}
		var toma map[string]any
		if i < len(tomas) {
			toma = asMap(tomas[i])
		}
		spans = append(spans, span{start: start, end: end, shot: shot, toma: toma})
	}
	if len(spans) == 0 {
		return raw, false
	}

	// Boundaries: 0, duration, every measured cut and every model cut.
	type point struct {
		t        float64
		measured bool
	}
	points := []point{{0, true}, {duration, true}}
	collect := func(t float64, measured bool) {
		if t > reconcileBoundaryTolerance && t < duration-reconcileBoundaryTolerance {
			points = append(points, point{t, measured})
		}
	}
	for _, cut := range measuredCuts {
		collect(cut, true)
	}
	for _, sp := range spans {
		collect(sp.start, false)
		collect(sp.end, false)
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].t < points[j].t })

	bounds := make([]float64, 0, len(points))
	for _, p := range points {
		if n := len(bounds); n > 0 && math.Abs(p.t-bounds[n-1]) <= reconcileBoundaryTolerance {
			continue
		}
		bounds = append(bounds, p.t)
	}
	if len(bounds) < 2 {
		return raw, false
	}
	// Snap each boundary onto a measured cut when one is close enough.
	for i := range bounds {
		best, bestDiff := bounds[i], reconcileBoundaryTolerance+1
		for _, cut := range measuredCuts {
			if d := math.Abs(cut - bounds[i]); d < bestDiff && d <= reconcileBoundaryTolerance {
				best, bestDiff = cut, d
			}
		}
		bounds[i] = best
	}

	changed := len(bounds)-1 != len(spans)
	if !changed {
		for i, sp := range spans {
			if !sameTime(sp.start, bounds[i], analyzeTimelineTolerance) || !sameTime(sp.end, bounds[i+1], analyzeTimelineTolerance) {
				changed = true
				break
			}
		}
	}
	if !changed {
		return raw, false
	}

	containing := func(mid float64) span {
		best := spans[0]
		bestDiff := math.MaxFloat64
		for _, sp := range spans {
			if mid >= sp.start && mid <= sp.end {
				return sp
			}
			d := math.Abs(mid - (sp.start+sp.end)/2)
			if d < bestDiff {
				best, bestDiff = sp, d
			}
		}
		return best
	}

	newShots := make([]any, 0, len(bounds)-1)
	newTomas := make([]any, 0, len(bounds)-1)
	durations := make([]float64, 0, len(bounds)-1)
	for i := 0; i+1 < len(bounds); i++ {
		start, end := bounds[i], bounds[i+1]
		if end-start < reconcileMinShot {
			continue
		}
		parent := containing((start + end) / 2)
		shot := deepCopy(parent.shot).(map[string]any)
		shot["index"] = float64(len(newShots) + 1)
		shot["start_s"] = start
		shot["end_s"] = end
		shot["duration_s"] = round2(end - start)
		shot["micro_shot"] = end-start < reconcileMicroShotSeconds
		shot["timing_precision"] = "approx"
		switch {
		case i+2 == len(bounds):
			shot["transition_out"] = map[string]any{
				"type": "none_end", "duration_s": 0.0, "direction": "none",
				"hidden_cut_cause": "none", "evidence": "", "purpose": "",
			}
		case sameTime(parent.end, end, analyzeTimelineTolerance):
			if tr, ok := parent.shot["transition_out"]; ok {
				shot["transition_out"] = deepCopy(tr)
			}
		default:
			shot["transition_out"] = map[string]any{
				"type": "hard_cut", "duration_s": 0.0, "direction": "none",
				"hidden_cut_cause": "none", "evidence": "corte medido por ffmpeg", "purpose": "",
			}
		}
		newShots = append(newShots, shot)
		durations = append(durations, end-start)

		toma := map[string]any{
			"n":      float64(len(newTomas) + 1),
			"tiempo": fmtSeconds(start) + "–" + fmtSeconds(end) + " s",
		}
		if parent.toma != nil {
			if v, ok := parent.toma["plano_y_camara"]; ok && strings.TrimSpace(toText(v)) != "" {
				toma["plano_y_camara"] = v
			}
			if v, ok := parent.toma["descripcion"]; ok && strings.TrimSpace(toText(v)) != "" {
				toma["descripcion"] = v
			}
		}
		if _, ok := toma["plano_y_camara"]; !ok {
			size, _ := shot["shot_size"].(string)
			move := ""
			if cm := asMap(shot["camera_movement"]); cm != nil {
				move, _ = cm["type"].(string)
			}
			toma["plano_y_camara"] = strings.TrimSpace(strings.Join(compactStrings(size, move), " · "))
		}
		if _, ok := toma["descripcion"]; !ok {
			toma["descripcion"], _ = shot["subject_action"].(string)
		}
		newTomas = append(newTomas, toma)
	}
	if len(newShots) == 0 {
		return raw, false
	}

	root["shots"] = newShots
	video["shot_count"] = float64(len(newShots))
	root["video"] = video
	legacy["numero_de_tomas"] = float64(len(newShots))
	legacy["duracion_s"] = duration
	legacy["tomas"] = newTomas
	root["legacy_ui"] = legacy

	editing := asMap(root["editing_style"])
	if editing == nil {
		editing = map[string]any{}
	}
	editing["metrics"] = computeMetrics(durations, duration)
	root["editing_style"] = editing

	quality := asMap(root["quality"])
	if quality == nil {
		quality = map[string]any{}
	}
	quality["checks_passed"] = true
	quality["timeline_source"] = "ffmpeg_measured"
	quality["reconciled"] = true
	quality["model_shots"] = float64(len(spans))
	quality["ffmpeg_cuts"] = float64(len(measuredCuts))
	quality["model_shots_list"] = originalShots
	assumptions := asSlice(quality["assumptions"])
	assumptions = append(assumptions, "Linea de tiempo reconstruida con los cortes medidos por ffmpeg (muestreo a 1 fps): las tomas añadidas heredan encuadre, movimiento y descripcion del plano que las contiene.")
	quality["assumptions"] = assumptions
	root["quality"] = quality

	out, err := json.Marshal(root)
	if err != nil {
		return raw, false
	}
	return out, true
}

// computeMetrics rebuilds the numbers that must match the final shot list.
func computeMetrics(durations []float64, total float64) map[string]any {
	n := len(durations)
	out := map[string]any{
		"shot_count":            float64(n),
		"cut_count":             float64(maxInt(n-1, 0)),
		"average_shot_length_s": 0.0,
		"median_shot_length_s":  0.0,
		"shortest_shot_s":       0.0,
		"longest_shot_s":        0.0,
		"cuts_per_second":       0.0,
		"micro_shot_count":      float64(0),
		"pace_class":            "medium",
		"pace_curve":            map[string]any{"quarters_asl_s": []any{}, "trend": "steady"},
	}
	if n == 0 || total <= 0 {
		return out
	}
	sum := 0.0
	micro := 0
	shortest, longest := durations[0], durations[0]
	for _, d := range durations {
		sum += d
		if d < reconcileMicroShotSeconds {
			micro++
		}
		if d < shortest {
			shortest = d
		}
		if d > longest {
			longest = d
		}
	}
	sorted := append([]float64(nil), durations...)
	sort.Float64s(sorted)
	median := sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	asl := sum / float64(n)
	out["average_shot_length_s"] = round2(asl)
	out["median_shot_length_s"] = round2(median)
	out["shortest_shot_s"] = round2(shortest)
	out["longest_shot_s"] = round2(longest)
	out["cuts_per_second"] = round2(float64(maxInt(n-1, 0)) / total)
	out["micro_shot_count"] = float64(micro)
	out["pace_class"] = paceClass(asl)
	out["pace_curve"] = paceCurve(durations, total, asl)
	return out
}

func paceClass(asl float64) string {
	switch {
	case asl < 0.5:
		return "very_fast"
	case asl < 1.0:
		return "fast"
	case asl < 2.5:
		return "medium"
	case asl < 5.0:
		return "slow"
	}
	return "very_slow"
}

// paceCurve averages shot length per quarter of the video (ported from the
// skill's metrics) and reports the trend between the first and last quarter.
func paceCurve(durations []float64, total, asl float64) map[string]any {
	quarters := make([][2]float64, 4)
	acc := [4]float64{}
	quarter := total / 4
	pos := 0.0
	for _, d := range durations {
		idx := int(pos / quarter)
		if idx > 3 || quarter <= 0 {
			idx = 3
		}
		acc[idx] += d
		quarters[idx][1]++
		pos += d
	}
	values := make([]any, 0, 4)
	means := make([]float64, 0, 4)
	for i := range acc {
		v := 0.0
		if quarters[i][1] > 0 {
			v = round2(acc[i] / quarters[i][1])
			means = append(means, v)
		}
		values = append(values, v)
	}
	trend := "steady"
	if len(means) >= 2 {
		first, last := means[0], means[len(means)-1]
		if asl > 0 {
			switch {
			case last < first*0.85:
				trend = "accelerating"
			case last > first*1.15:
				trend = "decelerating"
			}
		}
	}
	return map[string]any{"quarters_asl_s": values, "trend": trend}
}

func toText(v any) string {
	s, _ := v.(string)
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
