package agency

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// usageBodyLimit caps the response kept in the recovery trace: the analytics
// payload grows with the number of buckets and the dialog only needs the head.
const usageBodyLimit = 8000

// UsageEntry is one line of the provider usage analytics: one model, one API
// key, one bucket, with the generations it billed and their cost in USD.
type UsageEntry struct {
	Bucket     string  `json:"bucket,omitempty"`
	Model      string  `json:"model"`
	Unit       string  `json:"unit,omitempty"`
	Quantity   float64 `json:"quantity"`
	UnitPrice  float64 `json:"unit_price"`
	Cost       float64 `json:"cost"`
	Currency   string  `json:"currency,omitempty"`
	AuthMethod string  `json:"auth_method,omitempty"`
}

// UsageTrace records the exchange with Higgsfield's usage analytics route so
// the admin can compare a request's recovered spend against the figures the
// provider shows in its own billing dashboard.
type UsageTrace struct {
	URL          string       `json:"url"`
	Method       string       `json:"method"`
	StatusCode   int          `json:"status_code"`
	ResponseBody string       `json:"response_body"`
	Total        float64      `json:"total"`
	Lines        int          `json:"lines"`
	Entries      []UsageEntry `json:"entries,omitempty"`
	OK           bool         `json:"ok"`
	Error        string       `json:"error,omitempty"`
	DurationMs   int64        `json:"duration_ms"`
}

// ExactCost returns the charged amount when the window holds exactly one
// billed generation for this model (and API key, when the entry reports it):
// then the figure belongs to this task instead of being a window aggregate.
func (t UsageTrace) ExactCost(model, keyID string) (float64, bool) {
	var matched []UsageEntry
	generations := 0.0
	for _, entry := range t.Entries {
		if model != "" && entry.Model != "" && entry.Model != model {
			continue
		}
		if keyID != "" && entry.AuthMethod != "" && entry.AuthMethod != keyID {
			continue
		}
		matched = append(matched, entry)
		generations += entry.Quantity
	}
	if len(matched) != 1 || generations != 1 || matched[0].Cost == 0 {
		return 0, false
	}
	return matched[0].Cost, true
}

// TraceHiggsfieldUsage calls:
//
//	GET {baseURL}/models/usage?start=…&end=…&timeframe=hour&expand=auth_method
//
// which is the analytics behind the provider billing screen: completed
// generations of the account with their cost in USD, broken down per API key.
// Only completed generations are counted — failed, moderated and canceled
// requests are not charged — and the docs state this is analytics, not a
// billing record, so totals can lag and differ from the invoice. The
// credential never travels back: only the query and the bodies do.
func TraceHiggsfieldUsage(
	httpClient *http.Client,
	baseURL, authKey string,
	start, end time.Time,
) (trace UsageTrace) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	from, to := usageWindow(start, end)
	trace.Method = "GET"
	query := url.Values{}
	query.Set("start", from.Format(time.RFC3339))
	query.Set("end", to.Format(time.RFC3339))
	query.Set("timeframe", "hour")
	query.Set("expand", "auth_method")
	trace.URL = strings.TrimSuffix(baseURL, "/") + "/models/usage?" + query.Encode()

	started := time.Now()
	defer func() { trace.DurationMs = time.Since(started).Milliseconds() }()

	req, err := http.NewRequest("GET", trace.URL, nil)
	if err != nil {
		trace.Error = "build request: " + err.Error()
		return trace
	}
	req.Header.Set("Authorization", "Key "+authKey)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("[provider-usage] GET %s: %v", trace.URL, err)
		trace.Error = "request failed: " + err.Error()
		return trace
	}
	defer resp.Body.Close()
	trace.StatusCode = resp.StatusCode

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		trace.Error = "read body: " + err.Error()
		return trace
	}
	trace.ResponseBody = truncateBody(string(respBytes), usageBodyLimit)
	if resp.StatusCode >= 400 {
		log.Printf("[provider-usage] GET %s → %d: %.300s", trace.URL, resp.StatusCode, trace.ResponseBody)
		trace.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return trace
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(respBytes, &raw); err != nil {
		trace.Error = "unmarshal response: " + err.Error()
		return trace
	}
	trace.Entries = parseUsageEntries(raw)
	trace.Lines = len(trace.Entries)
	for _, entry := range trace.Entries {
		trace.Total += entry.Cost
	}
	trace.OK = true
	return trace
}

// usageWindow widens the submit instant to a 12h analytics window; a zero
// instant falls back to now.
func usageWindow(at, until time.Time) (time.Time, time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	if !until.IsZero() && until.After(at) {
		return at.Add(-time.Hour).UTC(), until.Add(time.Hour).UTC()
	}
	return at.Add(-6 * time.Hour).UTC(), at.Add(6 * time.Hour).UTC()
}

// parseUsageEntries flattens the time_series buckets of a /models/usage body.
// Costs ship as decimal strings ("11.2800"), so they are parsed leniently.
func parseUsageEntries(raw map[string]interface{}) []UsageEntry {
	var entries []UsageEntry
	series, _ := raw["time_series"].([]interface{})
	for _, bucket := range series {
		bucketMap, _ := bucket.(map[string]interface{})
		bucketName, _ := bucketMap["bucket"].(string)
		results, _ := bucketMap["results"].([]interface{})
		for _, result := range results {
			entry, _ := result.(map[string]interface{})
			parsed := UsageEntry{
				Bucket:     bucketName,
				Model:      stringField(entry, "model"),
				Unit:       stringField(entry, "unit"),
				Currency:   stringField(entry, "currency"),
				AuthMethod: stringField(entry, "auth_method"),
				Quantity:   HiggsfieldNumber(entry["quantity"]),
				UnitPrice:  HiggsfieldNumber(entry["unit_price"]),
				Cost:       HiggsfieldNumber(entry["cost"]),
			}
			if parsed.Cost == 0 {
				continue
			}
			entries = append(entries, parsed)
		}
	}
	return entries
}

func stringField(raw map[string]interface{}, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

// truncateBody keeps a response readable inside the trace dialog.
func truncateBody(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "… (truncado)"
}
