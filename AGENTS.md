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
- **`downloaded`** — weights executed by the inference worker (Python, gRPC;
  its own repo, `inference-worker`). These are **never defined in code**:
  every read goes live to the worker via the stateless client in
  `internal/worker` (dial per call,
  no cached catalog, no persisted state, no mock data). If the worker is
  unreachable, downloaded models are simply absent from listings — never
  invented.

Key rules when touching this area:

- Public listings (`GET /api/v1/models*`) serve usable models only: API
  models with a tenant credential plus worker models with `available: true`.
  `GET /api/v1/worker/models` is the admin management view and keeps
  unavailable entries with download progress and disk size.
- `GET /api/v1/models?type=downloaded` and `GET /api/v1/worker/status` must
  always hit the worker live (`internal/modules/model/live.go`); do not add
  caching, memoization or fallback fixtures there.
- The `tts` worker mode has no Synapta modality yet: it is listed with
  `modality: ""` and its raw mode preserved in `downloaded.mode`. Do not map
  it to `text`.
- `inference.proto` + generated code live in `internal/worker/proto`; they
  mirror the canonical contract in the `inference-worker` repo
  (`proto/inference.proto`, byte-identical). Copy the proto here and
  regenerate the Go stubs whenever the worker contract changes; the exact
  commands are in the worker README. (`brain-master`, the legacy gateway +
  old studio, is retired and gone from the workspace.)
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

## Per-tenant object storage (MinIO/S3)

Files live on the local disk (`UPLOAD_DIR`) unless the tenant configures its
own bucket: there is **no global bucket** in the environment and the back
never depends on one.

- Config: `storage_config` (one row per tenant, `migrations/tenant/00007`),
  managed by `GET/PUT/DELETE /api/v1/files/storage` (write = tenant admin,
  read = any member, secrets masked). Credentials are AES-256-GCM sealed with
  `ENCRYPTION_KEY` via `internal/secretbox`; the logic lives in
  `internal/modules/file/storage_config.go`.
- Saving validates the payload **and** the bucket end to end: `bucketProbe`
  signs a PUT of a throwaway key (`.probe/<uuid>.txt`), reads the bytes back
  and deletes it, so a broken endpoint/credential is never persisted. A plain
  HEAD to the bucket is not enough: the MinIO **console** (`console.<host>`)
  answers 200 with its SPA to every path and would silently swallow uploads.
  That is also why `s3Backend.Put` requires the `ETag` header S3 always
  returns. `enabled=false` keeps the row but reverts the tenant to local.
- `POST /api/v1/files/presign` signs a PUT with the tenant's bucket and
  returns 503 when there is none — the front then uses `POST /files/upload`
  (local disk). Keys: `<tenant>/<category>/<YYYY>/<MM>/<uuid><ext>`, only
  image/video/audio types, and `Content-Type` is signed.
- Reads: with `public_base` set (and verified public by the probe) `url` and
  `thumbnail_url` point straight at the bucket/CDN; otherwise they stay on
  `/api/v1/t/:slug/files/:id/serve`, which redirects bucket files to a fresh
  signature (`Cache-Control: private, max-age=300`). Never return a presigned
  URL in a response: it expires in 15 minutes.
- Signing lives in `internal/storage` (SigV4 query auth, no SDK), validated
  against AWS's published example and against a real MinIO.
- Endpoints are tenant-supplied and the server connects to them, so
  `guardPublicEndpoint` rejects loopback/private/link-local targets unless
  `STORAGE_ALLOW_PRIVATE_ENDPOINTS=true` (development MinIO). Keep it off in
  production.
- Tests: `TEST_DATABASE_URL=... go test ./internal/modules/file -run
  TestStorageConfig` applies the real migration to a disposable database;
  live uploads need `MINIO_TEST_ENDPOINT/BUCKET/ACCESS_KEY/SECRET_KEY`.
- `max_size` is advisory: S3 ignores size on a plain presigned PUT.

## Remote MCP server & assistant tool-calling

Two surfaces expose the same tenant-scoped domain operations:

- **`internal/modules/mcp`** — remote MCP server (Streamable HTTP, stateless
  JSON) at `POST/GET/DELETE /api/v1/mcp`, behind the same auth+tenant
  middleware as the rest of the API: clients send the normal login Bearer,
  no new credentials. Built on the official `modelcontextprotocol/go-sdk`.
- **`internal/modules/agent`** — the `/agency` assistant's direct fallback
  runs an agentic loop (`loop.go`, OpenAI + Anthropic wire formats) with the
  tools in `tools.go` (list/create/update projects, list models,
  generate video/image, list generations, cancel task). Emits SSE `tool`
  events for live front activity; caps at 6 rounds.

Both delegate to **`internal/toolkit`** — the shared domain operations
(find-or-create project/piece anchoring, generation slot numbering, live
catalog). Add domain capabilities THERE, not in either module.

Key rules:

- Tools operate on the per-request tenant bundle (schema-per-tenant pools):
  never add cross-tenant state. The assistant's in-memory conversation
  store is keyed by (tenant, user, conversation) on purpose.
- `generate_*` tools queue real provider spend — keep them behind the same
  refund/estimate paths as the REST endpoints (they call `GenerateUnified`).
- The LLM loop must honor a credential's custom `BaseURL` (gateways).

## Provider spend (cost tracking)

- Generation records carry `cost_credits`, `cost_usd`, `cost_source`
  (`provider_estimate` | `provider_refund`) and `transaction_id`.
- `EstimateHiggsfield` (`internal/modules/agency/estimate.go`) runs
  post-submit; every failure path logs `[estimate]` with status + response
  body — do not swallow those errors silently again, missing spend must be
  diagnosable from the server log.
- Failed/cancelled/NSFW tasks get refunded by the provider
  (`RefundProviderCostByTaskID`): zero cost + `cost_source='provider_refund'`.
- Clients render `cr` as provider credits (Higgsfield) next to the USD
  equivalent, with explanatory tooltips.

## Versionamiento antes de push

Si tocás un archivo de este repo, **actualizá la versión antes de hacer push**. No commitees código sin subir la versión correspondiente. Un cambio lógico por commit; la versión va en un commit `chore` separado del commit de código.

## Ops scripts (scripts/)

- `e2e_http.py` — full HTTP E2E (login → models → generate → artifact).
- `sync-remote-media.mjs` — pull uploads/outputs that only exist on the
  remote deployment into local storage. Idempotent, `--dry-run` supported.
  Requires creds in `../front-synappta/.env.e2e`.
- `deploy-sslip.sh` — deploy `bin/synapta-linux` to the remote host: scp,
  discover systemd vs bare process, back up the old binary, swap, restart,
  wait for `readyz=200`. Build first with
  `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/synapta-linux .`.
  Needs the SSH key authorized on the target host.
- `validate-worker-models.mjs` — valida si un back resuelve modelos
  "downloaded" (SD-1.5) vía el worker. Chequea `/readyz`,
  `/api/v1/worker/diagnostic`, `/api/v1/models?type=downloaded` y, opcional,
  `POST /api/v1/agency/image/generate`. Úsalo para comparar remoto vs local:
  `node scripts/validate-worker-models.mjs --remote <url> --local <url>
  --token <jwt>`.
- Never commit `.env`, `.env.e2e` or `*.log`; `bin/` and `*.log` are
  gitignored.
