package agency

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// usageBodyLimit caps the response kept in the recovery trace: the analytics
// payload grows with the number of buckets and the dialog only needs the head.
const usageBodyLimit = 8000

// usagePriceWindowDays is the widest window the analytics route accepts; the
// model's unit price is a catalog figure, so the lookup does not need to guess
// the submit hour.
const usagePriceWindowDays = 29

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

// UnitPrice returns the model's current billed unit price: the price of the
// newest bucket, so a past promo or a price change cannot distort the
// attribution. It fails when that bucket reports several prices at once, or
// none at all, because then the figure is not attributable.
func (t UsageTrace) UnitPrice(model, keyID string) (float64, bool) {
	newest := ""
	price, seen := 0.0, false
	for _, entry := range t.Entries {
		if model != "" && entry.Model != "" && entry.Model != model {
			continue
		}
		if keyID != "" && entry.AuthMethod != "" && entry.AuthMethod != keyID {
			continue
		}
		if entry.UnitPrice <= 0 {
			continue
		}
		switch {
		case entry.Bucket > newest:
			newest, price, seen = entry.Bucket, entry.UnitPrice, true
		case entry.Bucket == newest:
			if math.Abs(price-entry.UnitPrice) > 0.0000005 {
				return 0, false
			}
		}
	}
	return price, seen
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
) UsageTrace {
	from, to := usageWindow(start, end)
	query := url.Values{}
	query.Set("start", from.Format(time.RFC3339))
	query.Set("end", to.Format(time.RFC3339))
	query.Set("timeframe", "hour")
	query.Set("expand", "auth_method")
	return fetchUsage(httpClient, baseURL, authKey, query)
}

// TraceHiggsfieldModelPrice reads the model's billed unit price over the last
// month (timeframe=day). The price is a catalog figure, not a window
// aggregate, so a task's charge can be attributed as unit_price × units
// without knowing the exact submit bucket: that is how a generation stays
// recoverable when the account bills several of them inside the same hour.
func TraceHiggsfieldModelPrice(
	httpClient *http.Client,
	baseURL, authKey, model string,
) UsageTrace {
	end := time.Now().UTC()
	query := url.Values{}
	query.Set("start", end.AddDate(0, 0, -usagePriceWindowDays).Format(time.RFC3339))
	query.Set("end", end.Format(time.RFC3339))
	query.Set("timeframe", "day")
	query.Set("expand", "auth_method")
	if model != "" {
		query.Set("model", model)
	}
	return fetchUsage(httpClient, baseURL, authKey, query)
}

// fetchUsage performs the analytics call and flattens the time series.
func fetchUsage(
	httpClient *http.Client,
	baseURL, authKey string,
	query url.Values,
) (trace UsageTrace) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	trace.Method = "GET"
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
		log.Printf("[provider-usage] GET %s → %d: %.300s", trace.URL, trace.StatusCode, trace.ResponseBody)
		trace.Error = fmt.Sprintf("HTTP %d", trace.StatusCode)
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

// RequestStatusTrace records the status lookup of one provider request: the
// only per-generation identifier the API exposes. It reports the state, never
// the price, but a completed request is proof that the generation was billed.
type RequestStatusTrace struct {
	URL          string `json:"url"`
	Method       string `json:"method"`
	StatusCode   int    `json:"status_code"`
	ResponseBody string `json:"response_body"`
	RequestID    string `json:"request_id,omitempty"`
	Status       string `json:"status,omitempty"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
}

// TraceHiggsfieldRequestStatus calls:
//
//	GET {baseURL}/requests/{request_id}/status
//
// Failed, moderated and canceled requests are not charged, so a completed
// status is what makes a charge attributable to this generation.
func TraceHiggsfieldRequestStatus(
	httpClient *http.Client,
	baseURL, authKey, requestID string,
) (trace RequestStatusTrace) {
	if requestID == "" {
		trace.Error = "sin request_id"
		return trace
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	trace.Method = "GET"
	trace.RequestID = requestID
	trace.URL = strings.TrimSuffix(baseURL, "/") + "/requests/" + url.PathEscape(requestID) + "/status"

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
		log.Printf("[provider-request] GET %s: %v", trace.URL, err)
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
		log.Printf("[provider-request] GET %s → %d: %.300s", trace.URL, trace.StatusCode, trace.ResponseBody)
		trace.Error = fmt.Sprintf("HTTP %d", trace.StatusCode)
		return trace
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(respBytes, &raw); err != nil {
		trace.Error = "unmarshal response: " + err.Error()
		return trace
	}
	trace.Status = stringField(raw, "status")
	if id := stringField(raw, "request_id"); id != "" {
		trace.RequestID = id
	}
	trace.OK = trace.Status != ""
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
