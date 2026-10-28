package worker

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestDiagnosticAgainstRealWorker diagnostica el worker de inferencia de forma
// verídica: RPCs reales (Ping, ListModels, GpuTelemetry, GetModelState) contra
// un worker vivo, sin mocks ni cachés. Se salta si no hay worker configurado.
//
// Ejecutar:
//   BM_E2E_WORKER_ADDR=127.0.0.1:50051 go test ./internal/worker/ -run TestDiagnosticAgainstRealWorker -v
func TestDiagnosticAgainstRealWorker(t *testing.T) {
	addr := os.Getenv("BM_E2E_WORKER_ADDR")
	if addr == "" {
		t.Skip("set BM_E2E_WORKER_ADDR=<host>:<port> to test against a real worker")
	}

	c := NewClient()
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	// 1. Ping con latencia (el back usa este mismo llamado para readyz).
	t0 := time.Now()
	if err := c.Ping(ctx, addr); err != nil {
		t.Fatalf("worker no responde a Ping en %s: %v", addr, err)
	}
	pingMs := time.Since(t0).Milliseconds()

	// 2. ListModels: catálogo vivo, sin caché, sin invenciones.
	lm, err := c.ListModels(ctx, addr)
	if err != nil {
		t.Fatalf("ListModels falló en %s: %v", addr, err)
	}
	if lm == nil {
		t.Fatal("ListModels devolvió respuesta nula")
	}

	// 3. GpuTelemetry: telemetría real de GPU/VRAM.
	gt, err := c.GpuTelemetry(ctx, addr)
	if err != nil {
		t.Fatalf("GpuTelemetry falló en %s: %v", addr, err)
	}
	if gt == nil || gt.DeviceName == "" {
		t.Fatalf("GpuTelemetry sin device_name: %+v", gt)
	}

	// 4. GetModelState (catálogo entero): estado de pesos en disco.
	gs, err := c.GetModelState(ctx, addr, "")
	if err != nil {
		t.Fatalf("GetModelState falló en %s: %v", addr, err)
	}
	if gs == nil {
		t.Fatal("GetModelState devolvió respuesta nula")
	}

	available := 0
	for _, m := range lm.Models {
		if m != nil && m.Available {
			available++
		}
	}

	t.Logf("worker=%s ping_ms=%d modelos=%d disponibles=%d device=%s disco_bytes=%d",
		addr, pingMs, len(lm.Models), available, gt.DeviceName, gs.BytesOnDisk)

	if len(lm.Models) == 0 {
		t.Log("ADVERTENCIA: el worker responde pero no lista modelos. Revisá BM_WORKER_ADDR.")
	}
}
