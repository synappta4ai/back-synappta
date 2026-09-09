# Adding a New Video Generation Model to Synapta

This guide walks through every file and interface involved when integrating a new external video generation API into the agency pipeline.

---

## Architecture Overview

```
Request → Core.GenerateUnified()
  → model.ByName(catalog)        ← Step 1: catalog entry
  → Core.pickGenerator()
    → Generator.Match(modelName)  ← Step 2: PipelineRunner impl
  → Generator.Validate()
  → Generator.BuildPayload()
  → Generator.Generate()          ← POST to external API
  → Generator.GetStatus()         ← Poll until done
  → Generator.CancelTask()
  → CostCalculator.Calculate*()   ← Step 4: cost calc
  → GenerationSaver               ← persists output URL
```

All generators are registered at startup per-tenant in `internal/runtime/runtime.go`.

---

## Step 1 — Register the model in the catalog

**File:** `internal/modules/model/catalog.go`

Add a new entry to the `catalog` slice:

```go
{
    Name: "your-model-id", Modality: ModalityVideo,
    Generator: "your-generator-name",   // must match Generator.Name()
    CredentialProvider: ProviderBytePlus, // or add a new provider (see Step 5)
    GallerySync: false,                  // true if using BytePlus asset library
    DisplayName: "Your Model Name",
    Defaults: Defaults{
        Ratios:      []string{"16:9", "9:16", "1:1"},
        Resolutions: []string{"720p", "1080p"},
        Durations:   []int{5, 10},
    },
},
```

- `Name` is what clients send in generation requests.
- `Generator` is the name returned by your `PipelineRunner.Name()`.
- If your provider is not `byteplus`/`gemini`/`anthropic`, add it to `CredentialProvider` constants in this file and to `validProviders` in `credential.go`.

---

## Step 2 — Implement the PipelineRunner interface

**New file:** `internal/modules/agency/video/yourmodel.go`

The interface lives at `internal/modules/agency/types.go:134-143`:

```go
type PipelineRunner interface {
    Match(modelName string) bool
    Validate(req *GeneratorRequest) error
    Generate(req *GeneratorRequest) (*GeneratorResult, error)
    GetStatus(taskID, apiKey, baseURL, endpoint string) (*GeneratorResult, error)
    CancelTask(taskID, apiKey, baseURL, endpoint string) error
    BuildPayload(req *GeneratorRequest) map[string]interface{}
    ContentType() string
    Name() string
}
```

### Template

```go
package agencyvideo

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strings"
    "time"

    "synapta/config"
    "synapta/internal/modules/agency"
    "synapta/internal/utils"
)

type YourModelGenerator struct {
    httpClient *http.Client
    label      string
}

func NewYourModelGenerator() *YourModelGenerator {
    return &YourModelGenerator{
        httpClient: &http.Client{Timeout: 120 * time.Second},
        label:      "your-generator-name",  // must match catalog entry
    }
}

func (g *YourModelGenerator) Name() string      { return g.label }
func (g *YourModelGenerator) ContentType() string { return "video" }

func (g *YourModelGenerator) Match(modelName string) bool {
    return strings.Contains(strings.ToLower(modelName), "your-model-id")
}

func (g *YourModelGenerator) Validate(req *agency.GeneratorRequest) error {
    errs := agency.ValidateCommon(req)
    if errs.HasErrors() {
        return errs
    }
    // Add model-specific validation: duration range, supported ratios, etc.
    return nil
}

func (g *YourModelGenerator) Generate(req *agency.GeneratorRequest) (*agency.GeneratorResult, error) {
    payload := g.BuildPayload(req)
    result, err := g.doRequest(req.BaseURL+req.Endpoint, "POST", payload, req.APIKey)
    if err != nil {
        return nil, err
    }
    taskID, _ := result["id"].(string)
    if taskID == "" {
        taskID, _ = result["task_id"].(string)
    }
    if taskID == "" {
        return nil, fmt.Errorf("no task ID in response")
    }
    return &agency.GeneratorResult{
        TaskID:  taskID,
        Model:   req.Model,
        Status:  config.STATUS_RUNNING,
        Outputs: []agency.OutputResource{},
        Raw:     result,
    }, nil
}

func (g *YourModelGenerator) GetStatus(taskID, apiKey, baseURL, endpoint string) (*agency.GeneratorResult, error) {
    result, err := g.doRequest(baseURL+endpoint+"/"+taskID, "GET", nil, apiKey)
    if err != nil {
        return nil, err
    }
    status, _ := result["status"].(string)

    switch status {
    case config.STATUS_SUCCESS:
        videoURL := findYourModelVideoURL(result)
        if videoURL == "" {
            return &agency.GeneratorResult{
                TaskID: taskID, Status: "succeeded_no_url",
                Error: "Job succeeded but no video URL found.", Raw: result,
            }, nil
        }
        localName := fmt.Sprintf("yourmodel_%d_%s.mp4", time.Now().UnixMilli(), taskID)
        outputs := []agency.OutputResource{{URL: videoURL, Type: "video"}}
        localURL, err := utils.SaveURLOutput(videoURL, localName)
        if err == nil {
            outputs[0].LocalURL = localURL
        }
        return &agency.GeneratorResult{
            TaskID: taskID, Status: status, Outputs: outputs, Raw: result,
        }, nil
    case config.STATUS_FAILED:
        errorMsg, _ := result["error"].(string)
        return &agency.GeneratorResult{
            TaskID: taskID, Status: status, Error: errorMsg, Raw: result,
        }, nil
    default:
        return &agency.GeneratorResult{
            TaskID: taskID, Status: status, Raw: result,
        }, nil
    }
}

func (g *YourModelGenerator) CancelTask(taskID, apiKey, baseURL, endpoint string) error {
    _, err := g.doRequest(baseURL+endpoint+"/"+taskID, "DELETE", nil, apiKey)
    return err
}

func (g *YourModelGenerator) BuildPayload(req *agency.GeneratorRequest) map[string]interface{} {
    content := make([]map[string]interface{}, 0)
    textPart := agency.CompileContentText(req.Content)
    content = append(content, map[string]interface{}{
        "type": "text", "text": textPart,
    })
    for _, item := range req.Content {
        if item.Type == "image" && item.DataURL != "" {
            content = append(content, map[string]interface{}{
                "type": "image_url", "image_url": map[string]string{"url": item.DataURL},
            })
        }
        // add video/audio references as needed
    }
    return map[string]interface{}{
        "model":   "your-actual-api-model-id",
        "content": content,
        "duration": req.Duration,
        // ... other params your API needs
    }
}

// doRequest — HTTP helper (same pattern as SeedanceGenerator.arkRequest).
func (g *YourModelGenerator) doRequest(url, method string, body interface{}, apiKey string) (map[string]interface{}, error) {
    var bodyBytes []byte
    if body != nil {
        var err error
        bodyBytes, err = json.Marshal(body)
        if err != nil {
            return nil, fmt.Errorf("marshal: %w", err)
        }
    }
    req, err := http.NewRequest(method, url, bytes.NewReader(bodyBytes))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Authorization", "Bearer "+apiKey)
    req.Header.Set("Content-Type", "application/json")
    resp, err := g.httpClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    respBytes, err := io.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }
    if resp.StatusCode >= 400 {
        var result map[string]interface{}
        _ = json.Unmarshal(respBytes, &result)
        msg := agency.ExtractError(result, string(respBytes))
        return nil, fmt.Errorf("yourmodel %d: %s", resp.StatusCode, msg)
    }
    var result map[string]interface{}
    if err := json.Unmarshal(respBytes, &result); err != nil {
        return nil, fmt.Errorf("yourmodel: %s", string(respBytes))
    }
    return result, nil
}
```

### Key decisions when implementing

| Decision | Notes |
|---|---|
| **Async vs sync API** | If your API is sync (returns the video immediately), `Generate` can download the URL directly and return `Status: "succeeded"`. The reconciler only re-polls `running` tasks. |
| **Authentication** | Use the `apiKey` from credentials. If the API uses a different auth scheme (HMAC, OAuth), add signing logic similar to `signer.go`. |
| **Response parsing** | Write a `findYourModelVideoURL()` helper that walks the API JSON to extract the `.mp4` URL. See `findVideoURL()` in `video.go:321-358` for the pattern. |
| **Gallery sync** | Only needed for BytePlus models that reference uploaded assets. If your API accepts direct URLs, set `GallerySync: false`. |
| **Content types** | If your API supports image/video/audio references, add cases in `BuildPayload`. If it only supports text, skip the content loop. |

---

## Step 3 — Register the generator at startup

**File:** `internal/runtime/runtime.go`

Add an import and a `RegisterGenerator` call in the `build()` function (~line 241):

```go
import (
    // ... existing imports
    agencyvideo "synapta/internal/modules/agency/video"
)

// In build(), after existing RegisterGenerator calls:
core.RegisterGenerator(agencyvideo.NewYourModelGenerator())
```

---

## Step 4 — Implement a CostCalculator

**File:** `internal/modules/agency/calculators/calculators.go` (or new file)

The interface is at `internal/modules/agency/types.go:146-158`:

```go
type CostCalculator interface {
    Match(modelName string) bool
    CalculateFromResponse(raw interface{}, req *GeneratorRequest) (float64, bool)
    CalculateEstimated(req *GeneratorRequest) float64
    NeedsBackgroundCalc() bool
    Name() string
}
```

If your provider returns cost info in the response, implement `CalculateFromResponse`. Otherwise, compute it from duration/resolution parameters in `CalculateEstimated`.

Register in `runtime.go` (~line 249):

```go
core.RegisterCalculator(calculators.NewYourModelCalculator())
```

If you don't need custom cost logic, the `DefaultCalculator` (already registered) handles generic fallback.

---

## Step 5 — Add credential provider (if new)

**Files to modify:**

| File | Change |
|---|---|
| `internal/modules/model/catalog.go:24-27` | Add provider constant: `ProviderYourAPI CredentialProvider = "yourapi"` |
| `internal/modules/credential/credential.go:19-27` | Add `ProviderYourAPI = "yourapi"` constant and to `validProviders` map |

Users then set credentials via `PUT /credentials` with `provider: "yourapi"` and their API key in the `api_key` field (or `access_key_id`/`secret_access_key` for HMAC auth).

---

## Step 6 — (Optional) Add validation constants

**File:** `internal/modules/agency/video/yourmodel.go`

Define supported ratios/resolutions as package-level maps:

```go
var ValidRatiosYourModel = map[string]bool{
    "16:9": true, "9:16": true, "1:1": true,
}

var ValidResolutionsYourModel = map[string]bool{
    "720p": true, "1080p": true,
}
```

Use them in `Validate()`.

---

## File Checklist

| # | File | Action |
|---|---|---|
| 1 | `internal/modules/model/catalog.go` | Add model entry to `catalog` |
| 2 | `internal/modules/agency/video/yourmodel.go` | **New file.** Implement `PipelineRunner` |
| 3 | `internal/runtime/runtime.go` | Import + `RegisterGenerator()` |
| 4 | `internal/modules/agency/calculators/yourmodel.go` | **New file (or extend).** Implement `CostCalculator` |
| 5 | `internal/runtime/runtime.go` | `RegisterCalculator()` (if custom) |
| 6 | `internal/modules/model/catalog.go` | Add `CredentialProvider` constant (if new provider) |
| 7 | `internal/modules/credential/credential.go` | Add provider constant + `validProviders` entry (if new provider) |

---

## Testing

1. Add credentials via `PUT /credentials` for the provider.
2. `POST /agency/video/preview` with the new model name to verify payload construction.
3. `POST /agency/video/generate` to submit a real task.
4. `GET /agency/video/status/:taskId` to poll until completion.
5. Verify the downloaded MP4 in `./outputs/`.

---

## Common Pitfalls

- **`Match()` must be exclusive.** If two generators match the same model name, the first registered wins. Use specific string matching (e.g., exact name or prefix + no-suffix guard like Seedance does for `-gallery`).
- **`Name()` must match the catalog's `Generator` field exactly.** The `pickGenerator` dispatch relies on `Match()`, but the catalog links models to generators by name for documentation/lookup.
- **Status constants** come from `config/constants.go`. Use `config.STATUS_SUCCESS`, `config.STATUS_FAILED`, `config.STATUS_RUNNING`.
- **`BaseURL` and `Endpoint`** come from the tenant's credential record, not hardcoded. The core resolves them via `resolveProvider()`.
- **Async polling** happens every 2 minutes via the reconciler (`internal/modules/agency/reconciler.go`). Non-final tasks are automatically re-polled after server restart.
