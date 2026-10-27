// Cost backfill: recomputes missing generation spend from the stored request.
//
// Rows land with estimated_cost=0 when the provider /estimate call fails and
// no local calculator matched (e.g. higgsfield-seedance-2-5-reference before
// the Seedance25 matcher covered it). The payload needed to recompute lives
// in request_payload, so a login-triggered pass (POST
// /agency/tasks/backfill-costs) restores cost traceability without touching
// rows that already carry a provider estimate.
package agency

import (
	"encoding/json"
	"log"
	"strings"
)

// BackfillResult counts what a backfill pass saw and fixed.
type BackfillResult struct {
	Scanned int `json:"scanned"`
	Updated int `json:"updated"`
}

// backfillPayload mirrors the GenerateRequest fields needed to recompute a
// cost: shape (resolution/duration/ratio/quantity) plus content types.
type backfillPayload struct {
	Duration   float64 `json:"duration"`
	Resolution string  `json:"resolution"`
	Ratio      string  `json:"ratio"`
	Quantity   int     `json:"quantity"`
	Content    []struct {
		Type string `json:"type"`
	} `json:"content"`
}

// BackfillMissingCosts recomputes spend for rows without a usable cost
// (empty/pending source, or calculator with $0 on a non-free model) and
// fills empty video shape fields from the stored request. It never touches
// provider-reported rows (provider_estimate/refund) or Gemini rows (free).
// limit caps the pass; the front calls it on every login, so it stays cheap.
func (s *Core) BackfillMissingCosts(limit int) BackfillResult {
	res := BackfillResult{}
	if s.logStore == nil {
		return res
	}
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.logStore.ListCostBackfillCandidates(limit)
	if err != nil {
		log.Printf("[backfill] list candidates: %v", err)
		return res
	}
	res.Scanned = len(rows)
	for i := range rows {
		row := &rows[i]
		var payload backfillPayload
		if err := json.Unmarshal([]byte(row.Request), &payload); err != nil {
			continue
		}
		genReq := &GeneratorRequest{
			Model:      row.ModelName,
			Ratio:      firstNonEmpty(payload.Ratio, row.VideoRatio),
			Duration:   int(payload.Duration),
			Resolution: firstNonEmpty(payload.Resolution, row.VideoResolution),
			Quantity:   payload.Quantity,
		}
		if genReq.Duration == 0 {
			genReq.Duration = row.VideoDuration
		}
		for _, item := range payload.Content {
			if item.Type != "" {
				genReq.Content = append(genReq.Content, ContentItem{Type: item.Type})
			}
		}
		// Spend is only recomputed when a priced (non-default) calculator
		// matches: rows without known pricing keep their stored values so
		// local/free models are never rewritten. Refunded rows are already
		// excluded by the candidates query.
		var cost float64
		source := ""
		if calc := s.pickCalculator(row.ModelName); calc != nil && calc.Name() != "default" && !isFreeModel(row.ModelName) {
			if est := calc.CalculateEstimated(genReq); est > 0 && row.EstimatedCost == 0 {
				cost = est
				// Provider rows keep their source: only the missing USD
			// leg is completed (credits stay untouched).
				if row.CostSource != "provider_estimate" {
					source = "calculator"
				}
			}
		}
		dur, resName, ratio := row.VideoDuration, row.VideoResolution, row.VideoRatio
		if dur == 0 && genReq.Duration > 0 {
			dur = genReq.Duration
		}
		if resName == "" {
			resName = genReq.Resolution
		}
		if ratio == "" {
			ratio = genReq.Ratio
		}
		needsCost := cost > 0
		needsSource := source != "" && (row.CostSource == "" || row.CostSource == "pending" || row.CostSource == "calculator")
		needsMeta := (dur > 0 && row.VideoDuration == 0) ||
			(resName != "" && row.VideoResolution == "") ||
			(ratio != "" && row.VideoRatio == "")
		if !needsCost && !needsMeta && !needsSource {
			continue
		}
		if !needsCost {
			cost = 0
		}
		if !needsSource {
			source = ""
		}
		if err := s.logStore.ApplyCostBackfill(row.ID, cost, source, dur, resName, ratio); err != nil {
			log.Printf("[backfill] update %s: %v", row.TaskID, err)
			continue
		}
		res.Updated++
	}
	log.Printf("[backfill] scanned=%d updated=%d", res.Scanned, res.Updated)
	return res
}

// backfillCostFor recomputes a row's spend with the registered calculators.
// It reports whether the model has known (priced) costing. Kept for
// diagnosis: the pass above inlines the priced-only policy.
func (s *Core) backfillCostFor(modelName string, genReq *GeneratorRequest) (float64, string) {
	if isFreeModel(modelName) {
		return 0, ""
	}
	calc := s.pickCalculator(modelName)
	if calc == nil || calc.Name() == "default" {
		return 0, ""
	}
	if est := calc.CalculateEstimated(genReq); est > 0 {
		return est, "calculator"
	}
	return 0, "pending"
}

// isFreeModel reports models whose $0 cost is genuine (local/free tier),
// which the backfill must never rewrite to "pending".
func isFreeModel(modelName string) bool {
	lower := strings.ToLower(modelName)
	return strings.Contains(lower, "gemini")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
