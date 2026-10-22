# Modelos locales (Ollama y LM Studio)

Synapta puede generar texto con un LLM que corre en la máquina del tenant, sin
API key y sin costo por generación. Ambos servidores exponen la misma API
OpenAI-compatible, así que comparten un único generator (`openai-text`) y un
único camino de descubrimiento (`GET /v1/models`).

## Configurar

En **Admin → Modelos**, con el tenant seleccionado, guardá una credencial:

| Campo | Ollama | LM Studio |
|---|---|---|
| Proveedor | `ollama` | `lmstudio` |
| API key | *(no se pide)* | *(no se pide)* |
| Base URL | `http://localhost:11434/v1` | `http://localhost:1234/v1` |

- La Base URL se normaliza: si escribís sólo `http://localhost:11434`, el back
  le agrega `/v1`. Si ya incluye un path (gateway propio), se respeta.
- Levantar el servidor: `ollama serve` (+ `ollama pull qwen2.5:7b`), o en
  LM Studio **Developer → Start Server** (`lms server start`).
- *Alcance*: `localhost` es el loopback **del proceso back**. Si el back corre en
  otro host, usá un túnel (Tailscale/cloudflared) o corré el back en la misma
  máquina. Ollama no tiene autenticación propia: no lo expongas a Internet sin
  un proxy con auth.

## Cómo aparecen los modelos

Nada se declara a mano: `GET /api/v1/models` lee en vivo `GET {base_url}/models`
de cada servidor local configurado y agrega una entrada por modelo disponible.

- Nombre en el catálogo: **`<provider>/<modelo>`** — `ollama/qwen2.5:7b`,
  `lmstudio/qwen2.5-7b-instruct`. El prefijo es lo que enruta la generación.
- Modalidad: `text`. `type`: `api`. `generator`: `openai-text`.
- Si el servidor está caído o lento (>2 s), no aporta entradas y el resto del
  catálogo sigue funcionando. Nunca se inventan modelos ni se cachean.

## Generar

El pipeline común (`POST /api/v1/agency/text/generate`) usa el nombre completo:

```json
{
  "model": "ollama/qwen2.5:7b",
  "content": [{ "type": "text", "text": "Escribí un guion de 30 segundos" }],
  "event_id": "…", "piece_id": "…", "piece_code": "…", "generation_number": 1
}
```

- Es **síncrono**: `Generate` devuelve `status: "succeeded"` con el texto en
  `outputs[0].url` (`data:text/plain;base64,…`). `GetStatus`/`CancelTask` son
  no-ops.
- Timeout: 180 s (un 7B en CPU es mucho más lento que una API hospedada).
- Costo: el catálogo no tiene calculator para estos modelos, así que
  `DefaultCalculator` registra **0** — el gasto del tenant no se altera.
- `Authorization: Bearer <key>` sólo se envía si la credencial tiene una key.

## Problemas frecuentes

| Síntoma | Causa probable |
|---|---|
| El modelo no aparece en `/models` | Servidor apagado, otra URL, o falta la credencial |
| `model not found` al generar | Nombre sin prefijo (`qwen2.5:7b` en vez de `ollama/qwen2.5:7b`) |
| 404 en `/chat/completions` | Base URL sin `/v1` en un gateway que no soporta el default |
| Respuesta cortada | Timeout de 180 s: usá un modelo más chico o GPU |
| El asistente `/agency` no usa el modelo | El chat aún soporta sólo `openrouter`/`anthropic` |

## Mapa del código

| Archivo | Qué hace |
|---|---|
| `internal/modules/model/local.go` | Providers, default de puertos, normalización de URL, descubrimiento |
| `internal/modules/model/module.go` | Merge de los modelos locales en `GET /models` |
| `internal/modules/credential/credential.go` | Alta de credencial sin key, `LocalModelEndpoints` |
| `internal/modules/agency/models.go` | Lookup de nombres `<provider>/<modelo>` |
| `internal/modules/agency/text/openai_text.go` | Generator OpenAI-compatible |
| `internal/runtime/runtime.go` | Registro del generator (antes del de modelos descargados) |
