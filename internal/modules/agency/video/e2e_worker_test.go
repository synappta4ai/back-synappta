package agencyvideo

// Cross-language end-to-end tests against a REAL Python inference worker
// (brain-master) instead of the in-process Go fake.
//
// Skipped unless BM_E2E_WORKER_ADDR is set. Local run:
//
//	py -3 brain-master/inference-worker/run_mock.py      # worker gRPC :50051
//	BM_ARTIFACT_PORT=50052 py -3 .../run_mock.py         # + artifact server
//	BM_E2E_WORKER_ADDR=127.0.0.1:50051 \
//	  BM_E2E_ARTIFACT_BASE=http://127.0.0.1:50052 \
//	  go test ./internal/modules/agency/video/ -run TestE2EWorker -v

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"synapta/internal/worker"
)

func e2eAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("BM_E2E_WORKER_ADDR")
	if addr == "" {
		t.Skip("BM_E2E_WORKER_ADDR not set: skipping real-worker E2E")
	}
	return addr
}

func TestE2EWorkerListModels(t *testing.T) {
	addr := e2eAddr(t)

	c := worker.NewClient()
	resp, err := c.ListModels(context.Background(), addr)
	if err != nil {
		t.Fatalf("ListModels against real worker: %v", err)
	}
	if len(resp.Models) == 0 {
		t.Fatal("real worker reported zero models")
	}
	t.Logf("worker %s: device=%q cuda=%v models=%d", addr, resp.DeviceName, resp.CudaAvailable, len(resp.Models))

	var tiny bool
	for _, m := range resp.Models {
		if m.Key == "SD-Tiny-Test" {
			tiny = true
		}
	}
	if !tiny {
		t.Fatal("SD-Tiny-Test missing from the real worker catalog")
	}
}

func TestE2EWorkerVideoJob(t *testing.T) {
	addr := e2eAddr(t)
	artifactBase := os.Getenv("BM_E2E_ARTIFACT_BASE")
	if artifactBase == "" {
		artifactBase = "http://127.0.0.1:50052"
	}
	t.Setenv("BM_WORKER_ARTIFACT_BASE", artifactBase)
	t.Chdir(t.TempDir())

	g := NewDownloadedGenerator(worker.NewClient(), addr, ".")

	res, err := g.Generate(genRequest("SD-Tiny-Test"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("submitted job %s", res.TaskID)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, err := g.GetStatus(res.TaskID, "", "", "")
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		switch st.Status {
		case "succeeded":
			if len(st.Outputs) == 0 || st.Outputs[0].LocalURL == "" {
				t.Fatalf("succeeded without artifact: %+v", st)
			}
			// The artifact must exist on Synapta's side, fetched from the
			// worker (shared disk or artifact server) — never faked.
			name := filepath.Base(st.Outputs[0].LocalURL)
			data, err := os.ReadFile(filepath.Join("outputs", outputType(name), name))
			if err != nil {
				t.Fatalf("stored artifact unreadable: %v", err)
			}
			if len(data) == 0 {
				t.Fatal("stored artifact is empty")
			}
			t.Logf("artifact stored: %s (%d bytes, %q)", st.Outputs[0].LocalURL, len(data),
				strings.TrimSpace(string(data[:min(len(data), 40)])))
			return
		case "failed":
			t.Fatalf("job failed: %s (raw=%v)", st.Error, st.Raw)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the real worker to finish the job")
}

// min returns the smaller of two ints (Go <1.21 helper for older toolchains).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
