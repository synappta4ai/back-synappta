# AGENTS.md

Guidance for AI coding agents working in this repository.

## Add a new video model

Follow the step-by-step guide: `docs/ADDING_A_NEW_VIDEO_MODEL.md`.

It covers: catalog entry, generator, runtime registration, and cost calculator.

## Model types: api vs downloaded

The catalog (`internal/modules/model`) has two model types:

- **`api`** — external provider HTTP APIs (BytePlus, Gemini, Anthropic).
  Defined statically in `catalog.go`; each entry's `Generator` field links it
  to a `PipelineRunner`. To add one, follow `docs/ADDING_A_NEW_VIDEO_MODEL.md`.
- **`downloaded`** — weights executed by the brain-master inference worker
  (Python, gRPC). These are **never defined in code**: every read goes live
  to the worker via the stateless client in `internal/worker` (dial per call,
  no cached catalog, no persisted state, no mock data). If the worker is
  unreachable, downloaded models are simply absent from listings — never
  invented.

Key rules when touching this area:

- `GET /api/v1/models?type=downloaded` and `GET /api/v1/worker/status` must
  always hit the worker live (`internal/modules/model/live.go`); do not add
  caching, memoization or fallback fixtures there.
- The `tts` worker mode has no Synapta modality yet: it is listed with
  `modality: ""` and its raw mode preserved in `downloaded.mode`. Do not map
  it to `text`.
- `inference.proto` + generated code live in `internal/worker/proto`; they
  mirror brain-master's `proto/inference.proto` (regenerate if the worker
  contract changes).
- Config: `BM_WORKER_ADDR` (`host:port`, or `tls://host:port` for cloudflared
  tunnels) and `BM_WORKER_TIMEOUT_SECONDS` (per-call, default 6).

### Generate with downloaded models

`DownloadedGenerator` (`internal/modules/agency/video/downloaded.go`) is
registered LAST in `runtime.go` on purpose: it claims any model name outside
the API catalog. Flow:

- `Generate` submits `GenerateMedia` over gRPC and spawns a background stream
  consumer; the core treats the submit as an async task (`running`).
- The stream mirrors progress into an in-memory task store; on `COMPLETED` the
  artifact is re-hosted under Synapta's outputs (shared-disk read first,
  else `BM_WORKER_ARTIFACT_BASE/artifacts/<name>`). A missing artifact fails
  the task — never a fake success.
- `steps`/`cfg_scale` are sent as 0 so the worker applies its per-model
  defaults (the catalog spec, not duplicated here).
- Cross-language E2E (real Python worker, skipped without env):
  `BM_E2E_WORKER_ADDR=127.0.0.1:50051 BM_E2E_ARTIFACT_BASE=http://127.0.0.1:50052
  go test ./internal/modules/agency/video/ -run TestE2EWorker -v`
- Known limitation: after a Synapta restart the stream is gone, so an
  orphaned downloaded task polls as `running` forever (the worker does not
  expose a job-status-by-id RPC yet).
