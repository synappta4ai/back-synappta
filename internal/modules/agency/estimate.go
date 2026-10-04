package agency

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EstimateHiggsfield asks Higgsfield's official estimate route what a submit
// with the exact same payload would cost:
//
//	POST {baseURL}/estimate{endpoint}  →  {"credits":"1.500","usd":"0.094"}
//
// The docs (docs.higgsfield.ai/concepts/billing-and-retention) present the
// estimate returned for the authenticated account as the authoritative
// charge: successful generations are billed in credits, while failed, nsfw
// and canceled-queued requests are refunded. Both amounts are strings in the
// response, so they are parsed leniently. Returns ok=false when the estimate
// route is unavailable or the shape is unrecognized — callers must treat the
// estimate as best-effort and never block the generation on it.
func EstimateHiggsfield(httpClient *http.Client, baseURL, endpoint, authKey string, payload map[string]interface{}) (credits, usd float64, ok bool) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	estURL := strings.TrimSuffix(baseURL, "/") + "/estimate" + endpoint

	body, err := json.Marshal(payload)
	if err != nil {
		return 0, 0, false
	}
	req, err := http.NewRequest("POST", estURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, 0, false
	}
	req.Header.Set("Authorization", "Key "+authKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, 0, false
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode >= 400 {
		return 0, 0, false
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return 0, 0, false
	}

	credits = HiggsfieldNumber(result["credits"])
	usd = HiggsfieldNumber(result["usd"])
	if credits == 0 && usd == 0 {
		return 0, 0, false
	}
	return credits, usd, true
}

// HiggsfieldNumber parses a lenient JSON number that Higgsfield ships as a
// string ("1.500") or as a raw number. Unrecognized shapes parse as 0.
func HiggsfieldNumber(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return f
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(n, ",", "."))
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		if v != nil {
			if f, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil {
				return f
			}
		}
		return 0
	}
}
