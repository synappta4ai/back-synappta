# Inference providers hospedados (RunPod, Together, DeepInfra, …)

Synapta puede correr modelos en nubes de inferencia que el tenant paga por su
cuenta, además del catálogo curado y de los servidores locales
([MODELOS_LOCALES.md](MODELOS_LOCALES.md)). El tenant trae su API key; el
catálogo lista **en vivo** los modelos que esa key alcanza y no se cachea nada.

## Providers soportados

| Provider (slug) | Base URL por defecto | Texto | Imagen | Video | Cómo lista modelos |
|---|---|---|---|---|---|
| `runpod` | `https://api.runpod.ai` | — | ✅ | — | Sin listado: cada **Serverless Endpoint** es un modelo |
| `together` | `https://api.together.xyz/v1` | ✅ | ✅ `/images/generations` | — | `GET /v1/models` |
| `deepinfra` | `https://api.deepinfra.com/v1/openai` | ✅ | ✅ `/images/generations` | — | `GET …/models` |
| `gmi` | `https://api.gmi-serving.com/v1` | ✅ | — | — | `GET /v1/models` |
| `baseten` | `https://inference.baseten.co/v1` | ✅ | — | — | `GET /v1/models` |
| `vultr` | `https://api.vultrinference.com/v1` | ✅ | — | — | `GET /v1/models` |
| `digitalocean` | `https://inference.do-ai.run/v1` | ✅ | — | — | `GET /v1/models` |
| `coreweave` | `https://api.inference.wandb.ai/v1` | ✅ | — | — | `GET /v1/models` |
| `openrouter` | `https://openrouter.ai/api/v1` | ✅ | ✅ `/images` | ✅ `/videos` (async) | `GET /v1/models` + `GET /images/models` + `GET /videos/models` |

`gmi`, `baseten`, `vultr`, `digitalocean` y `coreweave` quedan como texto a
propósito: su ruta de imagen no es la de OpenAI (GMI usa una ruta de consola,
Baseten una URL por deployment), así que ofrecer modelos de imagen ahí
terminaría en un submit roto. `openrouter` también es el proveedor LLM del
asistente de agencia; la misma credencial sirve para ambas cosas.

## Configurar

En **Admin → Modelos**, con el tenant seleccionado:

1. Elegí el provider.
2. Pegá la **API key** (todas las nubes la requieren).
3. **Base URL** es opcional: sirve para gateways o regiones propias. Vacía = la
   de la tabla.
4. Solo RunPod: completá el **Endpoint ID** del Serverless Endpoint.

Guardar la credencial alcanza: al volver a listar modelos aparece lo que el
provider responde.

## Cómo se nombran los modelos

El nombre en el catálogo es **`<provider>/<modelo>`** y el prefijo es lo que
enruta la generación (el pipeline solo recibe un nombre, no un provider):

```
together/black-forest-labs/FLUX.1-schnell     → imagen (Together)
deepinfra/meta-llama/Llama-3.3-70B-Instruct   → texto (DeepInfra)
openrouter/bytedance-seed/seedream-4.5        → imagen (OpenRouter)
runpod/abc12def34gh                           → imagen (endpoint de RunPod)
```

- El id puede contener barras (`org/modelo`); el parser corta en la **primera**.
- La modalidad se clasifica por nombre (video antes que imagen: `veo`,
  `hailuo`, `wan-`, `kling`, … → video; `flux`, `seedream`, `image`,
  `nano-banana`, `krea`, … → imagen; el resto → texto). La misma función
  clasifica al listar y al generar, así que nunca se desincronizan. Video
  solo existe en OpenRouter: en los demás providers un nombre con pinta de
  video sigue siendo texto (suele ser un chat con entrada de video).
- OpenRouter además publica catálogos dedicados (`/images/models`,
  `/videos/models`) con capacidades reales (duraciones, resoluciones, aspect
  ratios): esas entradas mandan sobre la clasificación por nombre y rellenan
  los defaults del picker.
- Entradas de embeddings, rerankers y moderación se filtran del listado.

## RunPod en detalle

- Un **Serverless Endpoint** = un modelo. El nombre es
  `runpod/<endpoint-id>` y la ruta se arma como `/v2/<endpoint-id>`.
- El submit es **asíncrono**: `POST /v2/{id}/run` → job id; el core pollea
  `GET /v2/{id}/status/{job}` y cancela con `POST /v2/{id}/cancel/{job}`.
- Estados: `IN_QUEUE`/`IN_PROGRESS` → running, `COMPLETED` → succeeded,
  `FAILED`/`TIMED_OUT` → failed, `CANCELLED` → cancelled.
- **El schema lo define cada worker**, no RunPod. Este adapter habla el dialecto
  común de text-to-image:

  ```json
  { "input": { "prompt": "…", "width": 1024, "height": 1024,
               "seed": 42, "num_outputs": 2 } }
  ```

  La salida se busca recursivamente en `output`: URLs (`http…`) o base64 con
  cabecera de imagen. Un worker que solo acepte un `workflow` de ComfyUI
  necesita su propio adapter.
- Las imágenes base64 se guardan en `/outputs` y se sirven desde el back, igual
  que las URLs de cualquier otro provider (después se publican al bucket del
  tenant si lo tiene).

## Límites conocidos

- **Imágenes de referencia**: ni la ruta OpenAI de imágenes ni el dialecto de
  RunPod aceptan imágenes de entrada todavía; el request se rechaza con un
  error claro en lugar de ignorarlas.
- **Costos**: no hay calculator para estas nubes; `generation_logs` registra 0.
  El gasto real queda en la consola del provider.
- **Descubrimiento**: `GET /models` de cada provider se consulta en paralelo
  con timeout de 5 s. Si una nube está caída, aporta cero modelos y el resto
  del catálogo sigue funcionando. Nunca se inventan entradas.
- **Listado**: algunos providers devuelven cientos de modelos; el catálogo los
  muestra todos los que clasifiquen como texto o imagen.

## Mapa del código

| Archivo | Qué hace |
|---|---|
| `internal/modules/model/inference.go` | Registro de providers, parseo `<provider>/<modelo>`, clasificación y discovery |
| `internal/modules/model/module.go` | Merge de los modelos descubiertos en `GET /models` |
| `internal/modules/credential/credential.go` | Alta de credenciales, `InferenceEndpoints()` |
| `internal/modules/agency/models.go` | Resolución del nombre a modalidad/generator/ruta |
| `internal/modules/agency/text/openai_text.go` | Generator de texto OpenAI-compatible (local + hospedado) |
| `internal/modules/agency/image/openai_image.go` | Generator de imagen `/images/generations` |
| `internal/modules/agency/image/runpod.go` | Generator de RunPod serverless |
| `internal/runtime/runtime.go` | Registro de los generators |

## Probar

```bash
curl -s localhost:8099/api/v1/models?type=api -H "Authorization: Bearer $TOKEN" | jq '.[] | select(.credential_provider=="together")'
```

```json
{
  "model": "together/black-forest-labs/FLUX.1-schnell",
  "content": [{ "type": "text", "text": "un faro al atardecer" }],
  "ratio": "16:9",
  "event_id": "…", "piece_id": "…", "piece_code": "…", "generation_number": 1
}
```
