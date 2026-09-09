# Synapta

Backend **Go + Gin** orientado a eventos para generación IA: **eventos → programas → piezas → generaciones**, con generación de **video, imagen, texto y audio** (audio pendiente de generador).

## Arquitectura

- **Multi-tenant schema-per-tenant**: esquema `public` (tenants, usuarios, memberships) + `tenant_<slug>` por tenant (tablas de dominio). Pools por tenant creados perezosamente (`internal/tenancy`).
- **Modelos en código** (`internal/modules/model`): catálogo de solo lectura por modalidad (`video/audio/image/text`); las credenciales por proveedor son de cada tenant y van cifradas en reposo (`internal/modules/credential`).
- **Asignaciones polimórficas** (`assignments`): recursos (ingredientes, archivos, presets, skills...) se asocian a evento, programa o pieza por igual.
- **`agency`**: núcleo de orquestación por tenant (`GenerateUnified`) con generadores video (Seedance ×2), imagen (Seedream, Gemini ×2) y texto (Claude); logs completos en `generation_logs` + `server_communications` + `generated_assets`.
- **Reconciler**: barra cada 2 min las tareas no terminales de todos los tenants activos y las re-consulta, para que un reinicio no huérfe generaciones de vídeo en curso.

## Layout

```
synapta/
├── main.go                    # bootstrap + shutdown graceful + reconciler
├── config/                    # env con fail-fast en producción
├── migrations/
│   ├── system/                # goose sobre public
│   └── tenant/                # goose sobre cada tenant_<slug>
└── internal/
    ├── db/                    # pgx + pool limits + quote ident
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
        └── agency/            # core + video/ image/ text/ calculators/
```

## Arranque rápido

```bash
cp .env.example .env
docker compose up -d --build   # Postgres + API en :9099
# o local:
go run .
```

Health: `GET /healthz` · Readiness: `GET /readyz`

## Endpoints principales

```
POST /api/v1/auth/login                     # token con tenant_id
GET  /api/v1/models                         # catálogo de modelos (código)
PUT  /api/v1/credentials                    # credenciales del tenant (cifradas)

POST /api/v1/files/upload                   # dedup por SHA-256: contentido idéntico activo
                                            # → devuelve el existente (duplicate=true, HTTP 200);
                                            # ?force=true/1 fuerza una copia nueva (HTTP 201)

POST /api/v1/events  ·  GET /events/:id     # + /programs, /pieces, /generations
POST /api/v1/assignments/:targetType/:targetId

POST /api/v1/agency/video/generate          # + /status/:taskId, /task/:taskId, /preview
POST /api/v1/agency/image/generate
POST /api/v1/agency/text/generate
GET  /api/v1/agency/logs/generation         # logs y costes por tenant

GET  /api/v1/t/:slug/files/:id/serve        # público, rate-limited, con tenant explícito
```
