# Synapta

Backend **Go + Gin** orientado a eventos para generación IA: **eventos → programas → piezas → generaciones**, con generación de **video, imagen, texto y audio** y dos tipos de modelos distinguibles:

- **`api`** — proveedores externos (BytePlus, Gemini, Anthropic) con credenciales por tenant.
- **`downloaded`** — pesos ejecutados por el **inference worker de brain-master** (Python, gRPC), consultados **en vivo y sin caché**.

## Arquitectura

- **Multi-tenant schema-per-tenant**: esquema `public` (tenants, usuarios, memberships) + `tenant_<slug>` por tenant (tablas de dominio). Pools por tenant creados perezosamente (`internal/tenancy`).
- **Modelos con tipo** (`internal/modules/model`): catálogo de solo lectura por modalidad (`video/audio/image/text`).
  - `type: "api"`: definidos en código (`catalog.go`); credenciales por tenant cifradas en reposo (`internal/modules/credential`).
  - `type: "downloaded"`: **nunca en código ni en DB** — cada lectura va en vivo al worker vía `internal/worker` (cliente gRPC sin estado: dial por llamada, sin caché, sin persistencia, sin datos mock). Si el worker no responde, los modelos descargados simplemente no aparecen.
- **`agency`**: núcleo de orquestación por tenant (`GenerateUnified`) con generadores video (Seedance ×2), imagen (Seedream, Gemini ×2), texto (Claude) y **descargados** (gRPC al worker); logs completos en `generation_logs` + `server_communications` + `generated_assets`.
- **Reconciler**: barre cada 2 min las tareas no terminales de todos los tenants activos y las re-consulta, para que un reinicio no huérfe generaciones en curso.

### Flujo de un modelo descargado

```
POST /agency/video/generate (model: Wan2.1-T2V-1.3B)
  → agency.Core (LookupModel en vivo → type=downloaded)
    → DownloadedGenerator.Generate
      → gRPC GenerateMedia (job_id=syn_<ns>)
      └─ goroutine: stream LOADING_MODEL → GENERATING → COMPLETED
         └─ COMPLETED: artifact → outputs/video/syn_<ns>.mp4
GET /agency/video/status/:taskId   ← progreso real del stream (23%…100%)
GET /outputs/video/syn_<ns>.mp4    ← servido por Synapta (no expira)
```

- `steps`/`cfg_scale` van en `0`: el worker aplica sus defaults por modelo (spec del catálogo del worker, no duplicado acá).
- Recuperación del artifact: disco compartido (dev) o `BM_WORKER_ARTIFACT_BASE/artifacts/<nombre>` (worker remoto / túnel de Colab). Si no se puede traer, el job **falla** — nunca éxito falso.
- Los modelos descargados **no requieren credenciales** (corren en nuestra propia GPU).

## Layout

```
synapta/
├── main.go                    # bootstrap + shutdown graceful + reconciler
├── config/                    # env con fail-fast en producción
├── migrations/
│   ├── system/                # goose sobre public
│   └── tenant/                # goose sobre cada tenant_<slug>
├── scripts/
│   ├── e2e_http.py            # E2E HTTP: login → modelos → generar → artifact
│   ├── sync-remote-media.mjs  # trae al ./uploads local los medios que solo
│   │                          # existen en el despliegue remoto (idempotente)
│   └── deploy-sslip.sh        # deploy del binario linux al host remoto:
│                              # respalda, reemplaza, reinicia y checa /readyz
└── internal/
    ├── db/                    # pgx + pool limits + quote ident
    ├── worker/                # cliente gRPC stateless del inference worker
    │   └── proto/             # inference.proto + código generado (brain-master)
    ├── tenancy/               # registry, middleware, provisioner
    ├── runtime/               # bundle de servicios por tenant
    ├── middleware/            # JWT, rate limit (IP + usuario)
    ├── utils/
    └── modules/
        ├── registry.go        # Module interface + Registry
        ├── auth/ tenant/ model/ credential/
        ├── event/             # Event → Program → Piece → Generation
        ├── ingredient/ assignment/ preset/ skill/
        ├── file/ push/
        └── agency/            # core + video/ (incl. downloaded.go) image/ text/ calculators/
```

## Arranque rápido

```bash
cp .env.example .env
go run ./cmd/generate-keys     # genera JWT_SECRET, ENCRYPTION_KEY y claves VAPID
docker compose up -d db        # solo Postgres
go run .                       # API en :8099 (aplica migraciones al boot)
```

Health: `GET /healthz` · Readiness: `GET /readyz`

### Conectar el inference worker (modelos descargados)

En `.env`:

```bash
BM_WORKER_ADDR=127.0.0.1:50051          # o tls://xxx.trycloudflare.com (túnel Colab)
BM_WORKER_TIMEOUT_SECONDS=6             # timeout por llamada (lecturas siempre en vivo)
BM_WORKER_ARTIFACT_BASE=                # vacío en local; URL del artifact server si el worker es remoto
```

Worker local en modo mock (sin GPU, sin descargas — repo brain-master):

```bash
BM_ARTIFACT_PORT=50052 py -3 inference-worker/run_mock.py   # gRPC :50051 + artifacts :50052
```

Worker remoto (Colab T4 / VM): el worker anuncia su túnel y se usa esa dirección en `BM_WORKER_ADDR` (`tls://…` para cloudflared). Ver `deploy/README.md` de brain-master.

## Endpoints principales

```
POST /api/v1/auth/login                     # token con tenant_id
GET  /api/v1/models                         # catálogo combinado (?type=api|downloaded, ?modality=)
                                            #   "downloaded" se lee EN VIVO del worker: si no responde,
                                            #   no aparece (header X-Worker-Status: unreachable)
GET  /api/v1/worker/status                  # estado en vivo del worker (device, cuda, n° de modelos)
PUT  /api/v1/credentials                    # credenciales del tenant (cifradas) — solo modelos api

POST /api/v1/files/upload                   # dedup por SHA-256: contenido idéntico activo
                                            # → devuelve el existente (duplicate=true, HTTP 200);
                                            # ?force=true/1 fuerza una copia nueva (HTTP 201)
GET  /api/v1/files/by-event/:eventId        # recursos asignados a un proyecto, con
                                            # ingredients (character/location/prop) y project_ids
PUT  /api/v1/files/:id/event/:eventId       # asignar recurso a proyecto
DELETE /api/v1/files/:id/event/:eventId     # desasignar

POST /api/v1/events  ·  GET /events/:id     # + /programs, /pieces, /generations
POST /api/v1/assignments/:targetType/:targetId

POST /api/v1/agency/video/generate          # + /status/:taskId, /task/:taskId, /preview
POST /api/v1/agency/image/generate          #   modelos "downloaded": despachan gRPC al worker y
POST /api/v1/agency/text/generate           #   el artifact se re-hospeda en /outputs — mismo pipeline
GET  /api/v1/agency/logs/generation         # logs y costes por tenant
GET  /api/v1/agency/tasks/history           # ?from&to&resource_type&event_id&limit
GET  /api/v1/agency/tasks/server-communications  # ?task_id: detalle HTTP del proveedor
PATCH /api/v1/agency/tasks/rating           # rating excluyente: buena_toma / elegida_final

GET  /api/v1/t/:slug/files/:id/serve        # público, rate-limited, con tenant explícito
```

### Forma de un modelo en la API

```jsonc
// GET /api/v1/models?type=api
{ "name": "gemini-nano-banana", "modality": "image", "type": "api",
  "credential_provider": "gemini", "base_url": "https://…", "display_name": "Gemini Nano Banana", … }

// GET /api/v1/models?type=downloaded  (en vivo del worker)
{ "name": "Wan2.1-T2V-1.3B", "modality": "video", "type": "downloaded",
  "display_name": "Wan 2.1 T2V 1.3B (video ligero)",
  "downloaded": { "mode": "video", "engine": "diffusers", "pipeline": "wan",
                  "repo": "Wan-AI/Wan2.1-T2V-1.3B-Diffusers", "steps": 30,
                  "vram_gb": 6, "family": "wan", "available": true } }
```

## Costos del proveedor (spend)

Cada generación registra `cost_credits`, `cost_usd` y `cost_source` en
`generation_logs`:

- `provider_estimate` — estimado devuelto por el proveedor (Higgsfield
  responde `{"credits":"1.5","usd":"0.094"}` como strings). Los fallos de
  estimate se loguean con el tag `[estimate]` (status + cuerpo) en el log del
  servidor para poder diagnosticar costos faltantes.
- `provider_refund` — generación fallida/cancelada/nsfw: el proveedor
  reembolsa y el registro se pone a cero conservando la traza.
- Los clientes muestran `X.XX cr` (créditos del proveedor) + equivalente USD.

## E2E

Con el worker mock y la API levantados:

```bash
py -3 scripts/e2e_http.py    # login → modelos por tipo → worker/status →
                             # evento/pieza → generar (gRPC) → artifact
```

Tests Go (los E2E cross-language se saltean sin `BM_E2E_WORKER_ADDR`):

```bash
go test ./... -count=1

# Con el worker Python real (mock) corriendo:
BM_E2E_WORKER_ADDR=127.0.0.1:50051 \
BM_E2E_ARTIFACT_BASE=http://127.0.0.1:50052 \
  go test ./internal/modules/agency/video/ -run TestE2EWorker -v
```

## Docker (todo en uno)

```bash
docker compose up -d --build   # Postgres + API en :8099
```

En el contenedor, `BM_WORKER_ADDR` apunta al host con `host.docker.internal:50051`.
