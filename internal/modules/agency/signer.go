package agency

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ─── BytePlus AK/SK signing (asset library API) ─────────────────

// BytePlus asset library constants.
const (
	AssetsRegion  = "ap-southeast-1"
	AssetsService = "ark"
	AssetsVersion = "2024-01-01"
	AssetsHost    = "open.byteplusapi.com"
)

// SignedFetchInput contains all parameters for a signed AK/SK request.
type SignedFetchInput struct {
	AK      string
	SK      string
	Region  string
	Service string
	Action  string
	Version string
	Body    interface{}
	Method  string
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

func getDerivedKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte(secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("request"))
}

func uriEncode(s string, encodeSlash bool) string {
	encoded := ""
	for _, c := range []byte(s) {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			encoded += string(c)
		} else if c == '/' && !encodeSlash {
			encoded += "/"
		} else {
			encoded += fmt.Sprintf("%%%02X", c)
		}
	}
	return encoded
}

func buildCanonicalQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = uriEncode(k, true) + "=" + uriEncode(params[k], true)
	}
	return strings.Join(pairs, "&")
}

// SignedFetch makes a BytePlus API request using AK/SK signing (HMAC-SHA256).
func SignedFetch(input SignedFetchInput) (map[string]interface{}, error) {
	if input.AK == "" || input.SK == "" {
		return nil, fmt.Errorf("AK and SK are both required for signed requests")
	}

	method := input.Method
	if method == "" {
		method = "POST"
	}
	region := input.Region
	if region == "" {
		region = AssetsRegion
	}
	service := input.Service
	if service == "" {
		service = AssetsService
	}
	version := input.Version
	if version == "" {
		version = AssetsVersion
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := amzDate[:8]

	canonicalURI := "/"
	queryParams := map[string]string{
		"Action":  input.Action,
		"Version": version,
	}
	canonicalQuery := buildCanonicalQuery(queryParams)

	var bodyStr string
	if input.Body != nil {
		b, err := json.Marshal(input.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal body: %w", err)
		}
		bodyStr = string(b)
	}
	payloadHash := sha256Hex(bodyStr)

	headers := map[string]string{
		"content-type":     "application/json",
		"host":             AssetsHost,
		"x-date":           amzDate,
		"x-content-sha256": payloadHash,
	}

	signedHeadersList := make([]string, 0, len(headers))
	for k := range headers {
		signedHeadersList = append(signedHeadersList, k)
	}
	sort.Strings(signedHeadersList)

	canonicalHeaders := ""
	for _, h := range signedHeadersList {
		canonicalHeaders += h + ":" + headers[h] + "\n"
	}
	signedHeaders := strings.Join(signedHeadersList, ";")

	canonicalRequest := method + "\n" +
		canonicalURI + "\n" +
		canonicalQuery + "\n" +
		canonicalHeaders + "\n" +
		signedHeaders + "\n" +
		payloadHash

	algorithm := "HMAC-SHA256"
	credentialScope := dateStamp + "/" + region + "/" + service + "/request"

	stringToSign := algorithm + "\n" +
		amzDate + "\n" +
		credentialScope + "\n" +
		sha256Hex(canonicalRequest)

	signingKey := getDerivedKey(input.SK, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := algorithm + " Credential=" + input.AK + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature

	requestURL := "https://" + AssetsHost + "/?" + canonicalQuery

	req, err := http.NewRequest(method, requestURL, bytes.NewReader([]byte(bodyStr)))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", AssetsHost)
	req.Header.Set("X-Date", amzDate)
	req.Header.Set("X-Content-Sha256", payloadHash)
	req.Header.Set("Authorization", authorization)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signed request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		result = map[string]interface{}{"raw": string(body)}
	}

	if resp.StatusCode >= 400 {
		errMsg := extractAssetsError(result, string(body))
		return nil, fmt.Errorf("BytePlus signed request failed [%d]: %s", resp.StatusCode, errMsg)
	}

	if r, ok := result["Result"]; ok {
		if m, ok := r.(map[string]interface{}); ok {
			return m, nil
		}
	}
	return result, nil
}

func extractAssetsError(result map[string]interface{}, raw string) string {
	if rm, ok := result["ResponseMetadata"].(map[string]interface{}); ok {
		if e, ok := rm["Error"].(map[string]interface{}); ok {
			if msg, ok := e["Message"].(string); ok {
				return msg
			}
		}
	}
	if e, ok := result["Error"].(map[string]interface{}); ok {
		if msg, ok := e["Message"].(string); ok {
			return msg
		}
	}
	if msg, ok := result["message"].(string); ok {
		return msg
	}
	if len(raw) > 400 {
		raw = raw[:400]
	}
	return raw
}

// ─── AssetAPI ───────────────────────────────────────────────────

// AssetAPI wraps asset library operations using AK/SK.
type AssetAPI struct {
	AccessKeyID     string
	SecretAccessKey string
	AssetGroupID    string
	commStore       *ServerCommunicationStore
}

// NewAssetAPI builds an asset API client.
func NewAssetAPI(ak, sk, groupID string) *AssetAPI {
	return &AssetAPI{AccessKeyID: ak, SecretAccessKey: sk, AssetGroupID: groupID}
}

// SetCommStore attaches the trace store.
func (a *AssetAPI) SetCommStore(store *ServerCommunicationStore) { a.commStore = store }

func (a *AssetAPI) logComm(action, endpoint string, reqBody interface{}, resp map[string]interface{}, err error) {
	if a.commStore == nil {
		return
	}
	reqBytes, _ := json.Marshal(reqBody)
	var respBytes []byte
	if resp != nil {
		respBytes, _ = json.Marshal(resp)
	}
	errMsg := ""
	statusCode := 200
	if err != nil {
		errMsg = err.Error()
		statusCode = 0
	}
	_ = a.commStore.Create(&ServerCommunication{
		ID:           uuid.New().String(),
		ModelName:    AssetsService,
		Endpoint:     endpoint,
		Method:       "POST",
		RequestBody:  string(reqBytes),
		ResponseBody: string(respBytes),
		StatusCode:   statusCode,
		ErrorMessage: errMsg,
	})
	log.Printf("[asset-api] %s status=%d err=%q", action, statusCode, errMsg)
}

// CreateAssetGroup creates a new asset group in the BytePlus asset library.
func (a *AssetAPI) CreateAssetGroup(name, description, projectName string) (map[string]interface{}, error) {
	if projectName == "" {
		projectName = "default"
	}
	body := map[string]interface{}{
		"Name":        name,
		"Description": description,
		"GroupType":   "AIGC",
		"ProjectName": projectName,
	}
	result, err := SignedFetch(SignedFetchInput{
		AK: a.AccessKeyID, SK: a.SecretAccessKey,
		Region: AssetsRegion, Service: AssetsService,
		Action: "CreateAssetGroup", Version: AssetsVersion, Body: body,
	})
	a.logComm("CreateAssetGroup", "/?Action=CreateAssetGroup", body, result, err)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":          result["Id"],
		"name":        name,
		"description": description,
		"projectName": projectName,
	}, nil
}

// CreateAsset creates an asset in the library. Requires a publicly accessible URL.
func (a *AssetAPI) CreateAsset(assetURL, name, assetType, projectName string) (map[string]interface{}, error) {
	if assetType == "" {
		assetType = "Image"
	}
	if projectName == "" {
		projectName = "default"
	}
	if a.AssetGroupID == "" {
		return nil, fmt.Errorf("asset group ID not configured on this model")
	}

	payload := map[string]interface{}{
		"GroupId":     a.AssetGroupID,
		"URL":         assetURL,
		"AssetType":   assetType,
		"Name":        name,
		"ProjectName": projectName,
	}

	result, err := SignedFetch(SignedFetchInput{
		AK: a.AccessKeyID, SK: a.SecretAccessKey,
		Region: AssetsRegion, Service: AssetsService,
		Action: "CreateAsset", Version: AssetsVersion, Body: payload,
	})
	a.logComm("CreateAsset", "/?Action=CreateAsset", payload, result, err)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"id":        result["Id"],
		"status":    "Processing",
		"assetType": assetType,
	}, nil
}

// GetAsset polls the status of an asset in the library.
func (a *AssetAPI) GetAsset(assetID, projectName string) (map[string]interface{}, error) {
	if projectName == "" {
		projectName = "default"
	}
	body := map[string]string{"Id": assetID, "ProjectName": projectName}
	result, err := SignedFetch(SignedFetchInput{
		AK: a.AccessKeyID, SK: a.SecretAccessKey,
		Region: AssetsRegion, Service: AssetsService,
		Action: "GetAsset", Version: AssetsVersion, Body: body,
	})
	a.logComm("GetAsset", "/?Action=GetAsset", body, result, err)
	return result, err
}

// ─── Gallery model registry ─────────────────────────────────────

// GalleryModels lists model names that perform automatic gallery sync.
var GalleryModels = []string{
	"dreamina-seedance-2-0-gallery",
}

// IsGalleryModelName returns true if the model name matches a gallery model.
func IsGalleryModelName(modelName string) bool {
	lower := strings.ToLower(modelName)
	for _, m := range GalleryModels {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// BuildReferenceURI builds the model-specific reference URI:
// gallery models use asset://<id>, others use the CDN URL.
func BuildReferenceURI(modelName string, assetID, assetURL string) string {
	if IsGalleryModelName(modelName) {
		return "asset://" + assetID
	}
	return assetURL
}

// ─── Image normalization (BytePlus limits) ─────────────────────

// BytePlus validation limits for uploaded assets.
const (
	BytePlusMinHeight = 300
	BytePlusMaxHeight = 6000
	BytePlusMinAspect = 0.4
	BytePlusMaxAspect = 2.5
)

// DetectAssetType maps a MIME type to the BytePlus asset type.
func DetectAssetType(mimeType string) string {
	if strings.HasPrefix(mimeType, "video") {
		return "Video"
	}
	if strings.HasPrefix(mimeType, "audio") {
		return "Audio"
	}
	return "Image"
}

// IsImageMime reports whether a MIME type is an image.
func IsImageMime(mime string) bool {
	return strings.HasPrefix(mime, "image/")
}
