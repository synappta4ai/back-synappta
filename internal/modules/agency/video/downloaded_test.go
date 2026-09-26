package agencyvideo

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"synapta/internal/modules/agency"
	"synapta/internal/worker"

	inferencev1 "synapta/internal/worker/proto"
)

// fakeStreamWorker is an in-process gRPC worker for generator tests: it
// streams LOADING_MODEL → GENERATING → COMPLETED and writes a small fake
// artifact where the real worker would write the MP4.
type fakeStreamWorker struct {
	inferencev1.UnimplementedInferenceServiceServer
	outputDir string
	holdOpen  chan struct{} // non-nil: streams stay open until closed
}

func (f *fakeStreamWorker) GenerateMedia(req *inferencev1.MediaGenerationRequest,
	srv inferencev1.InferenceService_GenerateMediaServer,
) error {
	artifact := filepath.Join(f.outputDir, req.JobId+".mp4")
	if err := os.WriteFile(artifact, []byte("fake-mp4-bytes"), 0o644); err != nil {
		return err
	}

	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "LOADING_MODEL", Percentage: 2})
	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "GENERATING", Percentage: 50, TotalSteps: 4, CurrentStep: 2})
	// Wait for test-controlled release so progress events land in the store
	// before the terminal event.
	if f.holdOpen != nil {
		select {
		case <-f.holdOpen:
		case <-srv.Context().Done():
			return srv.Context().Err()
		}
	}
	_ = srv.Send(&inferencev1.GenerationProgressResponse{JobId: req.JobId, Status: "COMPLETED", Percentage: 100, OutputFilePath: artifact})
	return nil
}

func (f *fakeStreamWorker) GetGpuTelemetry(context.Context, *inferencev1.GpuTelemetryRequest) (*inferencev1.GpuTelemetryResponse, error) {
	return &inferencev1.GpuTelemetryResponse{DeviceName: "Tesla T4"}, nil
}

func (f *fakeStreamWorker) CancelGeneration(ctx context.Context, req *inferencev1.CancelRequest) (*inferencev1.CancelResponse, error) {
	return &inferencev1.CancelResponse{JobId: req.JobId, Accepted: true}, nil
}

// startStreamWorker serves f locally and returns its address.
func startStreamWorker(t *testing.T, f *fakeStreamWorker) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	inferencev1.RegisterInferenceServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func genRequest(model string) *agency.GeneratorRequest {
	return &agency.GeneratorRequest{
		Model:    model,
		Content:  []agency.ContentItem{{Type: "text", Text: "a golden retriever astronaut"}},
		Duration: 2,
		Ratio:    "16:9",
	}
}

func TestDownloadedGenerateHappyPath(t *testing.T) {
	// Outputs are written relative to OUTPUTS_DIR; isolate in a temp cwd.
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	res, err := g.Generate(genRequest("Wan2.1-T2V-1.3B"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != "running" {
		t.Fatalf("submit status = %q, want running", res.Status)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := g.GetStatus(res.TaskID, "", "", "")
		if err != nil {
			t.Fatalf("GetStatus: %v", err)
		}
		switch st.Status {
		case "succeeded":
			if len(st.Outputs) != 1 || st.Outputs[0].LocalURL == "" {
				t.Fatalf("succeeded without outputs: %+v", st)
			}
			if filepath.Ext(st.Outputs[0].LocalURL) != ".mp4" {
				t.Fatalf("artifact name = %q, want .mp4", st.Outputs[0].LocalURL)
			}
			return
		case "failed":
			t.Fatalf("job failed: %s", st.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for job completion")
}

func TestDownloadedGenerateImageMode(t *testing.T) {
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	// SD-1.5 is an API-catalog name, so LookupModel resolves it with modality
	// image: the generator must send mode="image" to the worker.
	res, err := g.Generate(genRequest("SD-1.5"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := g.GetStatus(res.TaskID, "", "", "")
		if st.Status == "succeeded" || st.Status == "failed" {
			if st.Status == "failed" {
				t.Fatalf("job failed: %s", st.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job never completed")
}

func TestDownloadedCancelTask(t *testing.T) {
	t.Chdir(t.TempDir())

	f := &fakeStreamWorker{outputDir: t.TempDir()}
	g := NewDownloadedGenerator(worker.NewClient(), startStreamWorker(t, f), ".")

	res, _ := g.Generate(genRequest("Wan2.1-T2V-1.3B"))
	if err := g.CancelTask(res.TaskID, "", "", ""); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
}

func TestDownloadedValidateFailsWithoutWorker(t *testing.T) {
	g := NewDownloadedGenerator(worker.NewClient(), "", t.TempDir())
	if err := g.Validate(&agency.GeneratorRequest{
		Model:   "X",
		Content: []agency.ContentItem{{Type: "text", Text: "p"}},
	}); err == nil {
		t.Fatal("expected validation failure with empty worker addr")
	}
}

func TestStoreArtifactRemoteFallback(t *testing.T) {
	t.Setenv("BM_WORKER_ARTIFACT_BASE", "")
	// No shared disk + no artifact base → "" (task fails, never fake success).
	if got := storeArtifact(filepath.Join(t.TempDir(), "missing.mp4"), "job1"); got != "" {
		t.Fatalf("storeArtifact = %q, want \"\"", got)
	}
}
