package agency

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	trace := TraceHiggsfieldEstimate(httpClient, baseURL, endpoint, payload, authKey)
	return trace.Credits, trace.USD, trace.OK
}

// EstimateTrace records the exact HTTP exchange with the provider estimate
// route so the admin can see which endpoint, payload and response produced a
// spend figure (and why it was rejected).
type EstimateTrace struct {
	URL          string  `json:"url"`
	Method       string  `json:"method"`
	RequestBody  string  `json:"request_body"`
	StatusCode   int     `json:"status_code"`
	ResponseBody string  `json:"response_body"`
	Credits      float64 `json:"credits"`
	USD          float64 `json:"usd"`
	OK           bool    `json:"ok"`
	Error        string  `json:"error,omitempty"`
	DurationMs   int64   `json:"duration_ms"`
}

// TraceHiggsfieldEstimate performs the estimate call and returns the full
// exchange (URL, payload, status, body) for diagnostics. The credential never
// travels back: only the endpoint and the bodies are reported.
func TraceHiggsfieldEstimate(
	httpClient *http.Client,
	baseURL, endpoint string,
	payload map[string]interface{},
	authKey string,
) (trace EstimateTrace) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	trace.Method = "POST"
	// The route is /estimate + endpoint; an endpoint stored without its leading
	// slash used to produce /estimatebytedance/... and a 404.
	path := endpoint
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	trace.URL = strings.TrimSuffix(baseURL, "/") + "/estimate" + path
	start := time.Now()
	defer func() { trace.DurationMs = time.Since(start).Milliseconds() }()

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[estimate] marshal payload: %v", err)
		trace.Error = "marshal payload: " + err.Error()
		return trace
	}
	trace.RequestBody = string(body)
	req, err := http.NewRequest("POST", trace.URL, strings.NewReader(trace.RequestBody))
	if err != nil {
		log.Printf("[estimate] build request: %v", err)
		trace.Error = "build request: " + err.Error()
		return trace
	}
	req.Header.Set("Authorization", "Key "+authKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[estimate] POST %s: %v", trace.URL, err)
		trace.Error = "request failed: " + err.Error()
		return trace
	}
	defer resp.Body.Close()
	trace.StatusCode = resp.StatusCode

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[estimate] read body: %v", err)
		trace.Error = "read body: " + err.Error()
		return trace
	}
	trace.ResponseBody = string(respBytes)
	if resp.StatusCode >= 400 {
		log.Printf("[estimate] POST %s → %d: %.300s", trace.URL, resp.StatusCode, trace.ResponseBody)
		trace.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return trace
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		log.Printf("[estimate] unmarshal response (%d): %.200s", resp.StatusCode, trace.ResponseBody)
		trace.Error = "unmarshal response: " + err.Error()
		return trace
	}

	trace.Credits = HiggsfieldNumber(result["credits"])
	trace.USD = HiggsfieldNumber(result["usd"])
	if trace.Credits == 0 && trace.USD == 0 {
		log.Printf("[estimate] response sin credits/usd reconocibles: %.300s", trace.ResponseBody)
		trace.Error = "response sin credits/usd reconocibles"
		return trace
	}
	trace.OK = true
	return trace
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
