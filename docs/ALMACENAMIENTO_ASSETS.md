# Assets, imágenes y videos: dónde vive cada byte

Este documento describe el recorrido completo de los binarios de Synapta: qué
sube el usuario a la biblioteca, qué generan los modelos y cómo se sirve todo,
**con bucket propio del tenant o sin él**.

Resumen de una línea: **la biblioteca (`/files`) y los generados van al bucket
del tenant cuando lo tiene configurado; sin bucket, todo se queda en disco**
(`UPLOAD_DIR` para la biblioteca, `OUTPUTS_DIR` para los generados, servidos
por el back desde `/outputs`). El bucket es siempre opcional.

---

## 1. Los dos almacenes

| Almacén | Qué guarda | Configurable por tenant | Servido por |
|---|---|---|---|
| **Biblioteca** (`files`) | subidas del usuario (imágenes, videos, audio, referencias del studio) y copias normalizadas temporales | **Sí**: bucket propio o disco local | `GET /api/v1/t/:slug/files/:id/serve` y `/thumbnail` |
| **Outputs** (`OUTPUTS_DIR` y bucket) | imágenes y videos generados por los modelos (API y worker local) | **Sí**: bucket propio o disco | Local: `GET /outputs/...` (estático). Bucket: `GET /api/v1/t/:slug/outputs/...` (302 a la firma o al base público) |

Cada tenant tiene su schema Postgres (`tenant_<slug>`) con su tabla `files`, y
todos comparten el mismo `UPLOAD_DIR` / `OUTPUTS_DIR` del proceso. La
separación de la biblioteca es lógica (filas por tenant) y, cuando hay bucket,
física (un bucket por tenant). Los outputs en disco **no** están separados por
tenant (el nombre incluye el `taskID` pero el path no); los que van al bucket
sí, porque cada tenant tiene el suyo y la key arranca con `outputs/`.

```
                     ┌──────────────────────────────┐
  navegador ────────►│ POST /api/v1/files/upload    │
  (multipart)        │  (auth + tenant middleware)  │
                     └──────────────┬───────────────┘
                                    │ ¿tenant tiene bucket habilitado?
                       ┌────────────┴────────────┐
                       │ no                      │ sí
                       ▼                         ▼
              UPLOAD_DIR/<cat>/<uuid>.ext   PUT prefirmado al bucket
              files.backend = "local"       files.backend = "s3"
                       │                         │
                       └────────────┬────────────┘
                                    ▼
                    GET /t/:slug/files/:id/serve
                    local → stream del disco
                    s3    → 302 a GET prefirmado (15 min)
```

Y en paralelo, la generación:

```
modelo API ──(URL firmada/base64)──┐
worker gRPC/bridge ──(artifact)────┤
                                   ▼
                         OUTPUTS_DIR/{image,video,audio}/...   ← siempre se descarga acá
                                   │ ¿tenant tiene bucket habilitado?
                       ┌───────────┴────────────┐
                       │ no                     │ sí
                       ▼                        ▼
          GET /outputs/... (estático)   PUT a <bucket>/outputs/...
          el archivo queda en disco     y se borra la copia local
                                        ▼
          GET /api/v1/t/:slug/outputs/... (302 a public base o firma de 15 min)
```

---

## 2. Biblioteca: subida (el flujo que usa el front hoy)

`POST /api/v1/files/upload` — multipart, requiere JWT y resuelve el tenant del
usuario. Handler en `internal/modules/file/file.go` (`Handler.Upload`), lógica
en `Service.Upload`.

Campos del formulario:

| Campo | Obligatorio | Valores | Notas |
|---|---|---|---|
| `file` | sí | binario | — |
| `category` | sí | `images`, `videos`, `audio`, `temp`, `character`, `location`, `props`, … | un solo segmento seguro: `[a-z0-9_-]`, máx. 32; forma parte del path |
| `storage` | no | `persistent` (default), `temp` | es *persistencia*, no backend |
| `force` | no | `true` / `1` | saltea el dedup |
| `event_id` | no | uuid de proyecto | asigna el archivo al proyecto (también en dedup) |

Pasos:

1. **Límite de tamaño.** Se lee el cuerpo completo con `io.LimitReader` hasta
   `MAX_UPLOAD_BYTES + 1` (10 MiB por defecto); si se pasa, **413** y no se
   escribe nada. `Service.Upload` vuelve a validar el tamaño.
2. **Validación de categoría** (segmento de path seguro, sin `..` ni `/`).
3. **Hash SHA-256 y dedup.** Si ya existe un archivo activo del tenant con los
   mismos bytes (y `force` no está), se devuelve **200 + `duplicate: true`**
   apuntando al existente, sin escribir nada.
4. **Elección de backend** (`uploadBackend`): bucket del tenant si
   `storage_config.enabled` y las credenciales descifran; si no, disco local.
5. **Escritura** de los bytes bajo `<category>/<uuid><ext>`.
6. **Fila en `files`** con `backend` = `local` o `s3`, `sha256`, `size`,
   `mime_type`, `format` y `duration` (esta última, leída del header del
   contenedor para videos MP4/MOV/M4V, WAV y MP3).
7. **Miniatura** (solo imágenes): 300×300 con `imaging.Fit`, PNG/JPEG según la
   extensión, guardada en `thumbnails/<category>/<uuid><ext>`. Si falla, la
   subida igual queda válida, pero sin `thumbnail_url`.
8. **Proyecto**: si vino `event_id`, se crea la fila en `event_files`.

Respuesta (`201`, o `200` si es duplicado):

```json
{
  "success": true,
  "data": {
    "id": "…uuid…",
    "filename": "foto.png",
    "url": "http://localhost:8099/api/v1/t/acme/files/…/serve",
    "thumbnail_url": "http://localhost:8099/api/v1/t/acme/files/…/thumbnail",
    "size": 123456,
    "mime_type": "image/png",
    "format": "png",
    "category": "images"
  }
}
```

Esas URLs son **públicas y sin JWT** (las usan `<img>`, `<video>` y las APIs
externas de generación), con rate limit 5 req/s y ráfaga 20. El detalle del
archivo (`GET /api/v1/files/:id`, listados) sí requiere JWT y además devuelve
`backend` y los `project_ids` / `ingredients` calculados.

### Dónde queda cada cosa

| Dato | Sin bucket | Con bucket |
|---|---|---|
| Bytes | `UPLOAD_DIR/<category>/<uuid><ext>` | `PUT` a `<bucket>/<category>/<uuid><ext>` |
| Miniatura | `UPLOAD_DIR/thumbnails/<category>/<uuid>.<ext>` | `PUT` a `<bucket>/thumbnails/<category>/<uuid>.<ext>` |
| Papelera (soft delete) | archivo y miniatura movidos a `UPLOAD_DIR/trash/...` | nada se mueve: solo se marca `trashed` |
| Metadatos | fila en `files` del schema del tenant | igual |
| Lectura pública | stream desde el disco (`c.File`) | **302** a `GET` prefirmado del objeto |
| Cache para consumidores internos | no hace falta | `UPLOAD_DIR/.cache/<category>/<uuid><ext>` |

`files.backend` se persiste por archivo justamente para que un archivo siga
resolviéndose a donde se escribió, aunque el tenant cambie de configuración
después (migración `migrations/tenant/00008_file_backend.sql`).

---

## 3. Biblioteca: el camino directo al bucket (presign)

`POST /api/v1/files/presign` (`internal/modules/file/presign.go`) entrega una
URL `PUT` prefirmada SigV4 para que el navegador suba **sin pasar por el back**:

- Solo existe si el tenant tiene bucket habilitado; si no, **503** con el texto
  `this tenant has no bucket configured; use /files/upload`.
- Key: `<slug-del-tenant>/<categoría>/<yyyy>/<mm>/<uuid><ext>`.
- Extensiones permitidas: imágenes (`jpg/jpeg/png/gif/webp`), video
  (`mp4/mov/webm/mkv`) y audio (`mp3/wav/m4a/ogg/flac`). Nada ejecutable
  (html/svg/js): el bucket sirve desde su propio origen.
- Se firma `Content-Type`, así que el cliente debe enviar exactamente el
  header devuelto.
- TTL de la URL: 15 minutos.

**Importante:** este endpoint **no crea la fila en `files`**. El objeto queda en
el bucket pero no aparece en la biblioteca ni tiene miniatura, y el front
actual **no lo usa** (sube todo por multipart). Tratarlo como una capacidad
disponible para integraciones futuras, no como el flujo vigente.

---

## 4. Biblioteca: lectura, edición y borrado

| Acción | Endpoint | Sin bucket | Con bucket |
|---|---|---|---|
| Servir bytes | `GET /t/:slug/files/:id/serve` | stream del disco | 302 a GET prefirmado |
| Miniatura | `GET /t/:slug/files/:id/thumbnail` | stream del disco | 302 a GET prefirmado |
| Detalle | `GET /files/:id` | fila + URLs | igual |
| Mover a papelera | `DELETE /files/:id` | mueve a `trash/` | marca `trashed` |
| Restaurar | `POST /files/:id/restore` | mueve de vuelta | limpia `trashed` |
| Borrado definitivo | `DELETE /files/:id/hard` | borra archivo, miniatura, `trash/…`, `.cache` y fila | borra objeto, miniatura, `.cache` y fila |
| Enlazar a proyecto | `PUT /files/:id/event/:eventId` | `event_files` | igual |
| Listados | `GET /files/page`, `/files`, `/files/by-event/:eventId` | filas + URLs calculadas | igual |

Detalles:

- La miniatura se genera **al subir** y, si falta (por ejemplo un archivo
  migrado a mano o un objeto borrado del bucket), se regenera on-demand desde
  el original la primera vez que se pide.
- Un archivo en papelera devuelve **410 Gone** en `serve`/`thumbnail`, y
  `GetServePath` falla con `file has been deleted`.
- Los consumidores internos (normalización de referencias para proveedores,
  sincronización de assets, toolkit) siguen trabajando con un *path* local:
  `GetServePath` descarga el objeto del bucket a `.cache/` la primera vez y
  reutiliza el cache después.
- La purga nocturna (cron a las 00:00) borra los archivos con
  `storage = 'temp'` de más de 30 días, tanto en disco como en bucket, y sus
  miniaturas. Comprende también las copias normalizadas que genera
  `SyncAsset`.
- Si un archivo tiene `backend = 's3'` y el tenant borró su configuración, el
  back responde **503** (`ErrStorageConfig`) en vez de servir otra cosa. Los
  archivos `local` no se ven afectados.

---

## 5. Assets generados por modelos de API

Los generadores (`internal/modules/agency/image`, `.../video`) no escriben en
la biblioteca: escriben en `OUTPUTS_DIR` y dejan rastro en
`generation_logs` (y en `piece_generations` cuando la generación pertenece a una
pieza de un proyecto).

Flujo típico de un video o imagen asíncrona:

1. `Generate` crea la tarea en el proveedor y una fila en `generation_logs`
   (`status`, `model`, `resource_type`, `piece_id`, `generation_number`).
2. Cada `GetStatus` actualiza progreso y metadatos (duración, fps, resolución,
   tokens) con `trackVideoMetadata`.
3. Al llegar a `SUCCESS`, `ensureLocalVideo` descarga la salida del proveedor a
   `OUTPUTS_DIR/<kind>/<kind>_<taskID><ext>` (`kind` = `image`, `video` o
   `audio`), para que el front no dependa de la URL firmada del proveedor que
   expira. Inmediatamente después, `publishLocalOutputs` sube ese archivo al
   bucket del tenant (key `outputs/<kind>/<archivo>`), reescribe
   `outputs[].url` a la ruta pública del tenant, deja `local_url` vacío y borra
   la copia local. Sin bucket (o si el PUT falla) no cambia nada: el output
   sigue en disco con su `/outputs/...`, igual que antes.
4. `saveToGeneration` persiste, si hay pieza y número, una fila en
   `piece_generations` con `video_url` (proveedor) y `video_local_url`
   (nuestra ruta `/outputs/...`), desactivando la generación anterior del mismo
   número.
5. La API de estado (`GetStatusUnified`) devuelve `outputs[0].url` como URL
   **absoluta**: la del bucket (`URL_PUBLIC + /api/v1/t/:slug/outputs/...`)
   cuando se publicó, o la del back (`URL_PUBLIC + /outputs/...`) si quedó en
   disco; `local_url` lleva la ruta relativa (queda vacía al publicar).

Nombres de archivo según el generador (todos dentro de `OUTPUTS_DIR`):

- **Gemini (imágenes inline)**: escribe `gemini_<id>_<i>.<ext>` directamente en
  la **raíz** de `OUTPUTS_DIR` (no en subcarpeta) y devuelve `/outputs/gemini_…`.
- **Seedance / Seedance 2.5 / Higgsfield**: al confirmar `SUCCESS` descargan la
  URL firmada a `video/seedance_<millis>_<taskID>.mp4`,
  `video/seedance25_<millis>_<taskID>.mp4` o
  `video/higgsfield_<millis>_<taskID>.mp4`.
- **Salidas asíncronas genéricas** (`ensureLocalVideo`, incluye imágenes de
  modelos asíncronos): `<kind>_<taskID><ext>` bajo `image/`, `video/` o
  `audio/`, con nombre estable por tarea (re-polls reutilizan el mismo archivo).
- **Modelos descargados (worker)**: el nombre es el que reportó el worker
  (`filepath.Base` del artifact), bajo la carpeta del tipo detectado por
  extensión.
- **Galerías**: `GET /api/v1/agency/images` y `/videos` listan
  `generation_logs` con `has_outputs`, filtrables por `event_id`, con la misma
  forma de `outputs`.

Servido: `main.go` expone `router.Static("/outputs", OUTPUTS_DIR)` para los
outputs que quedaron en disco. Es un estático **sin auth**: cualquiera con la
URL (que incluye el `taskID`, no adivinable) puede leerlo durante toda la vida
del archivo. Los publicados al bucket se sirven en
`/api/v1/t/:slug/outputs/...` (también público, ruta del módulo `file`): el
handler hace `302` al `public_base` del bucket si está configurado, o a una
firma GET fresca de 15 minutos. No hay borrado de outputs desde la API: los de
disco se limpian a mano.

---

## 6. Videos e imágenes de modelos descargados (worker)

Los modelos `downloaded` corren en la GPU del usuario o en un worker remoto
(`internal/modules/agency/video/downloaded.go`), por gRPC directo
(`BM_WORKER_ADDR`) o por la cola de bridge. El worker reporta en `COMPLETED` el
**path de su propio disco**; el back lo re-hospeda con `storeArtifact`:

1. **Disco compartido** (dev local): lee el archivo y lo copia a
   `OUTPUTS_DIR/<kind>/`.
2. **Worker remoto**: descarga
   `BM_WORKER_ARTIFACT_BASE + /artifacts/<nombre>` (servidor de artefactos del
   worker, `:50052` en el setup local) y lo guarda igual.
3. Si ninguna de las dos funciona, la tarea queda **FAILED** con el mensaje
   `artifact ... could not be retrieved from the worker`. Nunca se marca éxito
   sin bytes locales.

Igual que los modelos de API, el artefacto se descarga a `OUTPUTS_DIR` y, si
el tenant tiene bucket, se publica ahí con `publishLocalOutputs` (misma key
`outputs/<kind>/<archivo>` y borrado de la copia local).

---

## 7. Matriz "con bucket / sin bucket"

| Aspecto | Sin bucket (default) | Con bucket habilitado |
|---|---|---|
| Subidas nuevas de biblioteca | disco | bucket |
| Subidas viejas | disco | disco (siguen ahí) |
| Imágenes/videos generados | disco | **disco** (no se mueven) |
| URL pública de la biblioteca | igual | igual |
| `GET /files/:id/serve` | 200 stream | 302 prefirmado |
| Miniaturas | disco | bucket |
| Papelera | mueve archivos | solo cambia la fila |
| Ancho de banda de lectura | del back | del bucket |
| Requisitos de infra | nada | CORS `PUT` + `Content-Type`, política de lectura pública (o CDN en `public_base`), credenciales con permiso sobre el bucket |
| `POST /files/presign` | 503 | 200 con URL firmada |
| Configuración | ninguna | `PUT /api/v1/files/storage` (admin del tenant) |

Un tenant puede pasar de local a bucket en cualquier momento: los archivos
nuevos van al bucket y los viejos siguen sirviéndose desde disco, porque el
backend viaja en cada fila. Volver atrás (`DELETE /files/storage`) es posible
pero **deja huérfanos** los objetos que estaban en el bucket: no se descargan
ni se borran automáticamente, y los archivos que apuntaban a ellos dejan de
servirse (503) hasta restaurar la configuración.

---

## 8. Configuración

Variables de entorno del back (`.env`, ver `.env.example`):

| Variable | Default | Para qué |
|---|---|---|
| `UPLOAD_DIR` | `./uploads` | raíz de la biblioteca local, papelera, miniaturas y `.cache` |
| `OUTPUTS_DIR` | `./outputs` | raíz de lo generado, servido en `/outputs` |
| `MAX_UPLOAD_BYTES` | `10485760` (10 MiB) | tope del multipart (413) y del tamaño declarado en `/files/presign` |
| `STORAGE_ALLOW_PRIVATE_ENDPOINTS` | `false` | permite endpoints de bucket en loopback/redes privadas (MinIO de desarrollo); en producción está apagado porque el back se conecta al endpoint que cargue el admin del tenant |
| `BM_WORKER_ARTIFACT_BASE` | vacío | base del servidor de artefactos del worker remoto |
| `URL_PUBLIC` | `http://localhost:<PORT>` | base de las URLs absolutas que se devuelven al front |

Bucket por tenant (sin variables globales, se guarda cifrado en el schema del
tenant):

```bash
# requiere JWT de admin del tenant
curl -X PUT http://localhost:8099/api/v1/files/storage \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "endpoint": "https://minio.example.com",
    "bucket": "acme-media",
    "region": "us-east-1",
    "access_key_id": "…",
    "secret_access_key": "…",
    "public_base": "https://cdn.example.com/acme",
    "enabled": true
  }'
```

Antes de guardar, el back verifica el bucket de punta a punta: firma un `PUT`
de `.probe/<uuid>.txt`, vuelve a leer esos bytes y borra el objeto. Si las
credenciales o el bucket no existen, o si el endpoint no devuelve lo que se
acaba de escribir, la configuración no se persiste.

> El `endpoint` tiene que ser la **API S3/MinIO** (p. ej. `https://bucket.host`),
> no la consola web (`https://console.host`): la consola contesta `200` con su
> SPA a cualquier path, así que un `HEAD` "pasaba" y después cada archivo se
> perdía en silencio. Por la misma razón `s3Backend.Put` exige el `ETag` que
> S3 siempre devuelve al guardar.

Las credenciales quedan cifradas con AES-256-GCM (`ENCRYPTION_KEY`) y la API
las devuelve enmascaradas.

Validaciones del payload: `bucket` en minúsculas con el formato
`[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]` (3–63 caracteres, sin `_` ni mayúsculas),
`region` vacío equivale a `us-east-1`, `endpoint` debe ser `http`/`https` sin
query ni fragmento, y `enabled` se asume `true` si se omite. En producción el
endpoint no puede resolver a loopback ni a rangos privados (salvo
`STORAGE_ALLOW_PRIVATE_ENDPOINTS=true`).

`GET /files/storage` informa `configured`, `enabled`, `bucket`, `endpoint`,
`public_base` y si hay secret guardado; cualquier miembro del tenant puede
leerlo, solo el admin escribirlo.

### Lectura de los archivos: `public_base` vs redirect

- **Con `public_base`** (CDN o dominio público del bucket) `url` y
  `thumbnail_url` apuntan directo a `public_base/<key>`: el browser y el CDN
  leen del bucket, la API no participa por cada vista y la URL no caduca (por
  eso nunca se devuelven URLs prefirmadas: expiran en 15 min). El guardado
  verifica que ese dominio entregue el objeto **sin firma**; si el bucket es
  privado, la config se rechaza en vez de romper todas las imágenes.
- **Sin `public_base`** (o archivos en `local`) la URL sigue siendo
  `/api/v1/t/:slug/files/:id/serve|thumbnail`: para el bucket es un `302` a una
  firma fresca (los bytes no pasan por la API) con `Cache-Control: private,
  max-age=300`; para disco el API sirve el archivo.

---

## 9. Limitaciones conocidas

1. **Sin bucket, `OUTPUTS_DIR` crece.** La copia local se descarga siempre
   (aunque el tenant tenga bucket) y se borra recién cuando el PUT termina
   bien; si el bucket no está configurado, el disco del back acumula todos los
   generados y los sirve él.
2. **Outputs públicos.** `/outputs/...` (disco) y
   `/api/v1/t/:slug/outputs/...` (bucket) no piden auth; la privacidad depende
   de que el `taskID` no se filtre. Al publicar un output se deja
   `local_url = ""` a propósito: el front prefiere ese campo, así que cae a
   `url` (la del bucket) en vez de pedir un archivo local que ya no existe. Las
   filas viejas conservan su `local_url` y su archivo en disco.
3. **`/files/presign` no registra el objeto.** Sube al bucket pero no crea fila
   en `files`, así que no aparece en la biblioteca ni tiene miniatura.
4. **Dedup antes de elegir backend.** Subir bytes idénticos a un archivo viejo
   en disco devuelve ese archivo local aunque el tenant ya tenga bucket.
5. **Sin migración entre backends.** No hay backfill disco → bucket ni
   bucket → disco; borrar la configuración deja objetos huérfanos.
6. **`.cache` no tiene TTL.** El cache de `GetServePath` se borra solo con el
   borrado definitivo del archivo.
7. **Sin miniaturas de video.** Solo se generan para imágenes.
8. **Nomenclatura distinta entre caminos.** Multipart en bucket:
   `<category>/<uuid><ext>`. Presign: `<slug>/<category>/<yyyy>/<mm>/<uuid><ext>`.
   Local: `UPLOAD_DIR/<category>/<uuid><ext>`.
9. **TTL fijo.** Las URLs prefirmadas (PUT y GET) duran 15 minutos; no es
   configurable por tenant.
10. **Tamaño de generados sin tope.** Un video de salida grande se descarga
    entero a memoria antes de escribirse (`utils.SaveURLOutput`) y se vuelve a
    leer entero para publicarlo al bucket (`publishLocalOutputs`).

---

## 10. Mapa del código

| Pieza | Archivo |
|---|---|
| Modelo `files`, CRUD, dedup, subida multipart, ciclo de papelera, purga | `internal/modules/file/file.go` |
| Selección de backend local/s3 y contrato de blob | `internal/modules/file/backend.go` |
| Configuración de bucket por tenant (cifrado, probe, máscaras) | `internal/modules/file/storage_config.go` |
| Firma SigV4 y URLs prefirmadas | `internal/storage/presign.go` |
| Presign directo al bucket | `internal/modules/file/presign.go` |
| Miniaturas en memoria | `internal/modules/file/thumbnail.go` |
| Duración de audio/video | `internal/modules/file/mediainfo.go` |
| Migración `files.backend` | `migrations/tenant/00008_file_backend.sql` |
| Guardado de generados y `piece_generations` | `internal/modules/agency/progress.go`, `internal/modules/agency/core.go`, `internal/modules/event/event.go` |
| Artefactos de modelos descargados | `internal/modules/agency/video/downloaded.go` |
| Escritura/URLs de outputs en disco | `internal/utils/store.go`, `config/config.go` (`OutPutUrl`) |
| Publicación de outputs al bucket | `internal/modules/file/file.go` (`StoreOutput`, `OutputTarget`), `internal/modules/agency/progress.go` (`publishLocalOutputs`) |
| Sirve `/outputs` (disco) | `main.go` |
| Sirve `/api/v1/t/:slug/outputs/...` (bucket) | `internal/modules/file/file.go` (`ServeOutput`) |
| Tests del flujo | `internal/modules/file/upload_bucket_test.go`, `upload_backend_test.go`, `storage_config_test.go`, `internal/storage/presign_test.go` |
