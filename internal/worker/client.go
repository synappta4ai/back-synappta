// Package worker is the stateless gRPC client for the brain-master
// inference worker (Python).
//
// Stateless by design — "nothing cached":
//   - No persistent connection is kept. Every call dials the worker, runs the
//     RPC and drops the connection.
//   - No model catalog, telemetry or worker state is stored in memory. Every
//     read goes live to the worker; if the worker is down, callers get an
//     error (never a stale copy).
//   - No weights, artifacts or generation outputs are stored by Synapta.
//
// Address formats (BM_WORKER_ADDR):
//   - host:port        → plaintext gRPC (localhost or rented-VM deployments)
//   - tls://host:port  → gRPC over TLS (cloudflared tunnels from Colab)
package worker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	inferencev1 "synapta/internal/worker/proto"
)

// Default call timeout: the catalog and telemetry RPCs are cheap reads, but a
// tunneled worker can have real latency. Fail fast so HTTP callers degrade
// gracefully instead of hanging.
const DefaultTimeout = 6 * time.Second

// Client performs one-shot calls against the inference worker.
// It holds no connection and no state; the zero value is ready to use.
type Client struct{}

// NewClient returns a stateless worker client.
func NewClient() *Client { return &Client{} }

// call dials addr, runs fn against a fresh service client and closes the
// connection. The context must already carry the per-call deadline.
func (c *Client) call(ctx context.Context, addr string,
	fn func(inferencev1.InferenceServiceClient) error,
) error {
	creds := grpc.WithTransportCredentials(insecure.NewCredentials())
	if rest, ok := strings.CutPrefix(addr, "tls://"); ok {
		addr = rest
		creds = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))
	}
	if addr == "" {
		return fmt.Errorf("worker address is empty (set BM_WORKER_ADDR)")
	}

	// grpc.NewClient is lazy (no I/O), so dial errors surface at the first RPC
	// through ctx/fn — the deferred Close covers every early-return path.
	conn, err := grpc.NewClient(addr, creds)
	if err != nil {
		return fmt.Errorf("dial worker %s: %w", addr, err)
	}
	defer conn.Close()
	return fn(inferencev1.NewInferenceServiceClient(conn))
}

// ListModels returns the live catalog reported by the worker: every model the
// worker can run, with its modality, engine, VRAM cost and availability.
// No copy is kept on the Synapta side.
func (c *Client) ListModels(ctx context.Context, addr string) (*inferencev1.ListModelsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	var resp *inferencev1.ListModelsResponse
	err := c.call(ctx, addr, func(api inferencev1.InferenceServiceClient) error {
		out, err := api.ListModels(ctx, &inferencev1.ListModelsRequest{})
		if err != nil {
			return fmt.Errorf("ListModels: %w", err)
		}
		resp = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// GpuTelemetry returns the worker's current GPU/VRAM snapshot.
func (c *Client) GpuTelemetry(ctx context.Context, addr string) (*inferencev1.GpuTelemetryResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	var resp *inferencev1.GpuTelemetryResponse
	err := c.call(ctx, addr, func(api inferencev1.InferenceServiceClient) error {
		out, err := api.GetGpuTelemetry(ctx, &inferencev1.GpuTelemetryRequest{})
		if err != nil {
			return fmt.Errorf("GetGpuTelemetry: %w", err)
		}
		resp = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Ping probes the worker cheaply (telemetry RPC) to report reachability.
func (c *Client) Ping(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	return c.call(ctx, addr, func(api inferencev1.InferenceServiceClient) error {
		_, err := api.GetGpuTelemetry(ctx, &inferencev1.GpuTelemetryRequest{})
		return err
	})
}

// StreamEvent is one progress event of a GenerateMedia stream.
type StreamEvent struct {
	Percent   float64
	Status    string // LOADING_MODEL | GENERATING | POSTPROCESSING | COMPLETED | FAILED | CANCELLED
	OutputPath string // worker-side artifact path (set on COMPLETED)
	ErrMsg    string
}

// Terminal reports whether the event ends the generation stream.
func (e StreamEvent) Terminal() bool {
	switch e.Status {
	case "COMPLETED", "FAILED", "CANCELLED":
		return true
	}
	return false
}

// StreamGeneration dials the worker and consumes one GenerateMedia stream to
// its terminal event. The context governs the whole call: cancel it to drop
// the stream (the job keeps running worker-side; use CancelGeneration to stop
// it). No connection or job state is kept after the call returns.
func (c *Client) StreamGeneration(ctx context.Context, addr string,
	req *inferencev1.MediaGenerationRequest, onEvent func(StreamEvent),
) error {
	return c.call(ctx, addr, func(api inferencev1.InferenceServiceClient) error {
		stream, err := api.GenerateMedia(ctx, req)
		if err != nil {
			return fmt.Errorf("GenerateMedia: %w", err)
		}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("stream: %w", err)
			}
			ev := StreamEvent{
				Percent:    float64(resp.Percentage),
				Status:     resp.Status,
				OutputPath: resp.OutputFilePath,
				ErrMsg:     resp.ErrorMessage,
			}
			onEvent(ev)
			if ev.Terminal() {
				return nil
			}
		}
	})
}

// CancelGeneration asks the worker to abort a running job. accepted=false
// means the worker has no active job with that ID.
func (c *Client) CancelGeneration(ctx context.Context, addr, jobID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	var accepted bool
	err := c.call(ctx, addr, func(api inferencev1.InferenceServiceClient) error {
		resp, err := api.CancelGeneration(ctx, &inferencev1.CancelRequest{JobId: jobID})
		if err != nil {
			return err
		}
		accepted = resp.Accepted
		return nil
	})
	return accepted, err
}
