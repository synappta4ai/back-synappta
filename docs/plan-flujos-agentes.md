# PLAN — FLUJOS POR AGENTE (comercial · inmobiliaria · cine)

> Completa las instrucciones madre (`agente-comercial.md`, `agente-inmobiliaria.md`,
> `agente-cine.md`) con la parte de plataforma: qué pasa en cada paso, qué skills
> (herramientas) tiene cada agente, qué se persiste y qué ve el usuario.
> Cada agente tiene **su propio chat** y **su propia lista de skills por paso**.
> Los nombres de campos de ENTRADA/SALIDA se ajustan a los reales del código
> (los .md los dejaron como "esperados").

---

## 1. ESTADO ACTUAL (qué ya está cableado y qué falta)

| Pieza | Estado | Dónde |
|---|---|---|
| Chat propio por agente (ruta + persona) | ✅ | `internal/modules/agent/agent.go` (`/agent/chat/{commercial,real-estate,cinema}`), `personas.go` |
| Historial de conversación aislado por tenant+usuario | ✅ | `direct.go` (`convStore`) |
| Loop de tool-calling (OpenRouter + Anthropic) | ✅ | `loop.go`, eventos SSE `tool` |
| Skills/herramientas (8 tools vía toolkit) | ⚠️ set único para los 3 agentes y todos los pasos | `tools.go`, `internal/toolkit` |
| Artefacto estructurado `sales_angles` (Paso 1 inmobiliaria) | ✅ (contrato corto: title/hook/pitch/audience) | `direct.go` `extractAngles` |
| Contratos ricos de los 3 pasos de los .md | ❌ no cableados | solo en `docs/agente-*.md` |
| System prompts por paso (REGLAS COMUNES + MÓDULO + PASO N) | ❌ no cableados (hoy 2 prompts cortos por agente) | `personas.go` |
| Estado `necesita_informacion` (preguntas críticas) | ❌ | — |
| Persistencia del flujo entre sesiones | ❌ solo en memoria del front | `agency.component` `persistWorkflow()` |
| Módulo skill (system prompts CRUD por tenant) | ⚠️ existe pero no se aplica a los agentes | `internal/modules/skill` |
| Front: wizard de 4 pasos | ⚠️ pasos 2 y 3 deterministas (plantillas), no LLM | `agency.component.ts` |
| `agency-real-estate.component` (eventos `storyboard`, `element_registry`, `shot_breakdown`) | ❌ legacy: el back ya no emite esos eventos | unificar en el wizard |

---

## 2. ARQUITECTURA COMÚN

### 2.1 Máquina de estados del flujo

Un flujo = (tenant, usuario, agente, proyecto). Cuatro estados:

```
Paso 0 DOCUMENTACIÓN → Paso 1 PROMPTS/ÁNGULOS → Paso 2 STORYBOARD → Paso 3 ESCENAS
        (brief)              (artefacto LLM)         (artefacto LLM        (artefacto LLM
                                                        + generate_image)     + generate_video)
```

Cada agente nombra sus pasos a su manera (la UI traduce):

| Paso | Comercial | Inmobiliaria | Cine |
|---|---|---|---|
| 0 | Brief / Documentación | Proyecto inmobiliario | Guion / Documentación |
| 1 | Prompts de escena | Ángulos de venta | Análisis del guion (escenas) |
| 2 | Storyboard | Storyboard | Storyboard |
| 3 | Escenas (clips) | Escenas (clips) | Escenas (clips) |

**Transiciones**: cada paso avanza solo con `estado: "ok"` del artefacto **y**
aprobación explícita del usuario (botón Aprobar). `necesita_informacion` detiene
el flujo y vuelca las preguntas al chat; el siguiente mensaje del usuario reintenta
el mismo paso con el contexto acumulado. Volver atrás nunca borra artefactos
aprobados (regla ya implementada en `generateStoryboard()` del front).

### 2.2 System prompt por paso

Armado en el back (nuevo `prompts.go` en `internal/modules/agent`), leyendo las
secciones de los .md embebidos como `//go:embed` (o constantes):

```
system = "## REGLAS COMUNES" [+ "## MÓDULO DE ACTUACIÓN" si el agente lo tiene]
       + "## PASO N — …"          ← la sección completa del .md correspondiente
       + contexto del flujo       ← estado actual: brief, artefactos anteriores, modelo elegido
```

Los .md ya están escritos con encabezados fijos precisamente para esto. El
`contexto del flujo` evita que el LLM repita preguntas ya respondidas y le da
la biblia de continuidad (cine) o el ángulo elegido (storyboard).

### 2.3 Contrato SSE unificado (por paso)

Eventos existentes (`meta`, `text`, `tool`, `error`) + generalización del
artefacto:

| Evento | Cuándo | Payload |
|---|---|---|
| `meta` | apertura | `{conversation_id}` |
| `text` | chat libre | `{text}` |
| `tool` | cada invocación | `{name, status, summary, task_id, error}` |
| `artifact` | respuesta a un turno de paso | `{step: 1|2|3, kind, data}` — `data` = el JSON del contrato del .md |
| `questions` | `estado: necesita_informacion` | `{questions: ["… con sugerencia"]}` |
| `error` | fallo | `{message}` |

`sales_angles` se mantiene como alias de `artifact{step:1, kind:"sales_angles"}`
durante la migración del front; tras unificar se retira.

### 2.4 Persistencia del flujo

Tabla nueva en el schema del tenant:

```sql
CREATE TABLE agency_flows (
  id           TEXT PRIMARY KEY,          -- uuid
  user_id      BIGINT NOT NULL,
  agent        TEXT NOT NULL,             -- commercial | real_estate | cinema
  project_id   TEXT REFERENCES events(id) ON DELETE SET NULL,
  state        JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_agency_flows_user_agent ON agency_flows (user_id, agent);
```

`state` guarda el flujo completo: `{step, brief, artifacts: {1:…, 2:…, 3:…},
approvals: {…}, conversation_id}`. El toolkit gana `GetFlow/SaveFlow`; el chat y
el front leen/escriben por `(user, agent)` — el usuario retoma donde quedó y
cada agente recuerda lo suyo por separado.

### 2.5 Skills (herramientas) por agente y por paso

Hoy `agentTools()` devuelve lo mismo para todos. Nuevo `toolsFor(agent, step)`:

| Skill | Pasos | Notas |
|---|---|---|
| `list_projects` | 0 | ya existe |
| `create_project` | 0 | ya existe (el brief crea el proyecto) |
| `update_project` | 0–3 | ya existe |
| `get_project_brief` | 1–3 | **nueva**: lee `agency_flows.state.brief` + datos del proyecto |
| `save_artifact` | 1–3 | **nueva**: persiste el artefacto validado en `state` |
| `ask_user` | 1–3 | **nueva**: emite SSE `questions` y termina el turno |
| `list_models` | 2–3 | ya existe |
| `generate_image` | 2 | ya existe (hoja de storyboard) |
| `generate_video` | 3 | ya existe (clip) |
| `list_generations` | 2–3 | ya existe |
| `cancel_task` | 2–3 | ya existe |

Los tres agentes comparten el toolkit pero **cada uno expone su sublista por
paso** (p. ej. el Paso 1 no ve `generate_video`: en análisis solo se analiza).
La diferencia entre agentes no es el código de las skills sino los system
prompts y los contratos de artefacto.

El módulo `skill` queda como override opcional por tenant: si el tenant guardó
un skill con nombre `agent.<agente>.paso<n>`, su `system_prompt` reemplaza a la
sección del .md (personalización sin tocar código). Default: sección embebida.

### 2.6 Request del chat (extensión mínima)

`chatRequest` ya tiene `Artifact string`. Se aceptan los valores por paso:

```json
{"conversation_id":"…","message":"…","provider":"openrouter",
 "workflow":"commercial","artifact":"scene_prompts|storyboard|clip_prompt|sales_angles",
 "model":"…"}
```

`artifact` vacío = chat libre (hoy). El `workflow` lo fija la URL (ya implementado).

### 2.7 Límites del turno de artefacto

Los artefactos ricos (3 prompts con beats; un `prompt_video` de hasta 600
palabras) no caben en los 2048 tokens actuales: los turnos de artefacto usan
`max_tokens = 4096` (constante por defecto configurable). El chat libre mantiene 2048.

---

## 3. FLUJO COMERCIAL (paso a paso)

### Paso 0 — BRIEF (sin LLM)
- **Trigger**: el usuario carga marca/producto, sube archivos (producto, locación, poses) y elige `cantidad_prompts`, `registro`, modelo "cerebro".
- **Back**: `create_project` ancla el proyecto (evento); las fotos quedan en `files` y sus ids viajan en `referencias_subidas`.
- **UI**: formulario (ya existe en el wizard).

### Paso 1 — PROMPTS DE ESCENA (`artifact: scene_prompts`)
- **System**: REGLAS COMUNES + MÓDULO DE ACTUACIÓN + PASO 1 (agente-comercial.md).
- **Entrada** (del front, en el `message` o en un campo nuevo `payload`):
  `{nombre_proyecto, contexto, cantidad_prompts, registro, referencias_subidas[]}`.
- **Skills**: `get_project_brief`, `save_artifact`, `ask_user`.
- **Salida** (contrato del .md): `{paso:1, estado, preguntas_pendientes[], prompts[{id, titulo, resumen_para_usuario, mensaje_marca, locacion, personajes[], producto_en_escena, modo, registro, beats[{momento, verbo, accion_fisica, microgesto, dialogo, importancia}]}], fotos_sugeridas[], supuestos[], advertencias[]}`.
- **Validación back**: cantidad exacta de `prompts`; `beats` no vacíos; si `estado=necesita_informacion` → evento `questions` y fin de turno.
- **UI**: cards de prompts seleccionables; `fotos_sugeridas` alimentan la lista "subí esta foto" de cada escena del paso 2.

### Paso 2 — STORYBOARD (`artifact: storyboard`, por prompt elegido)
- **System**: REGLAS + MÓDULO + PASO 2.
- **Entrada**: `{prompt (la escena del paso 1), referencias[{tag, archivo, descripcion, funcion}], cantidad_vinetas, duracion_total_s, aspecto, duracion_minima_vineta_s}`.
- **Skills**: `generate_image` (hoja), `list_models`, `save_artifact`, `ask_user`.
- **Salida**: `{paso:2, estado, prompt_id, duracion_total_s, vinetas[{n, tipo_toma, que_se_ve, posiciones, miradas, accion, microgesto, dialogo, movimiento_camara, referencias[], duracion_s, importancia}], prompt_hoja_storyboard, fotos_faltantes[], …}`.
- **Validación back**: `len(vinetas) == cantidad_vinetas`; `sum(duracion_s) == duracion_total_s`; ninguna `< duracion_minima_vineta_s`.
- **UI**: por escena: viñetas del artefacto + botón "Generar hoja" que llama `generate_image` con `prompt_hoja_storyboard` + `reference_image_ids` (ya existe la modal; hoy el prompt se arma en el front con plantilla → pasa a venir del artefacto). Aprobación de la hoja obligatoria para avanzar.

### Paso 3 — ESCENAS / CLIPS (`artifact: clip_prompt`, por escena aprobada)
- **System**: REGLAS + MÓDULO + PASO 3.
- **Entrada**: `{prompt, vinetas, referencias[{tag, archivo, tipo: storyboard|personaje|producto|locacion|utileria|movimiento|voz}], modelo_video, duracion_total_s, aspecto, resolucion, sonido, registro, modo}`.
- **Skills**: `list_models`, `generate_video`, `list_generations`, `save_artifact`, `ask_user`.
- **Salida**: `{paso:3, estado, prompt_id, duracion_total_s, prompt_video (11 bloques etiquetados), uso_de_referencias[], continuidad{…}, notas_editor{locucion, eslogan, musica, orden_sugerido}, …}`.
- **Validación back**: cada `tag` de `referencias` aparece al menos una vez en `prompt_video`; tiempos que suman `duracion_total_s`.
- **UI**: un prompt por segmento de duración del modelo (ya existe `VideoPrompt[]`); "Generar clip" llama `generate_video` con `prompt_video` + `reference_image_ids` (storyboard + fotos de la escena); `notas_editor` se muestra en la card para el editor.

---

## 4. FLUJO INMOBILIARIA (paso a paso)

### Paso 0 — PROYECTO
Igual que comercial: proyecto + fotos (fachada, amenidades, entorno) + datos
del insumo (PDF/folleto → texto extraído en `contexto`).

### Paso 1 — ÁNGULOS DE VENTA (`artifact: sales_angles`, **extendido**)
- **System**: REGLAS COMUNES + PASO 1 (agente-inmobiliaria.md). Sin módulo de actuación.
- **Entrada**: `{nombre_proyecto, ubicacion, contexto, cantidad_angulos}`.
- **Skills**: `get_project_brief`, `save_artifact`, `ask_user`.
- **Salida**: contrato del .md — se **extiende** el `extractAngles` actual con:
  `{paso:1, estado, proyecto{nombre, ubicacion, datos_clave[]}, angulos[{id, titulo, promesa, virtudes_agrupadas[], comprador_ideal, gancho_cierre, alternativas_cierre[], fotos_sugeridas[{descripcion, motivo, prioridad}], **+ title/hook/narrative_pitch/target_audience legacy**}], datos_faltantes[], …}`.
  Los campos legacy se rellenan derivados (title=titulo, hook=gancho_cierre,
  narrative_pitch=promesa, target_audience=comprador_ideal) hasta migrar el front.
- **Validación**: cantidad exacta; sin promesas financieras (chequeo de regex
  simple sobre `promesa`: "rentabilidad", "plusvalía", "garantizado" → `advertencias`).
- **UI**: cards de ángulos seleccionables (ya existe); `fotos_sugeridas` guían la subida.

### Paso 2 — STORYBOARD (por ángulo elegido)
Mismo contrato que comercial §Paso 2, con las diferencias del .md inmobiliario:
- `vinetas[].posiciones` puede ser "sin personas" (M5); las personas solo si el ángulo lo pide.
- La foto asignada a cada viñeta es obligatoria (`referencias[tag]`); si falta → `fotos_faltantes` y la UI bloquea "Generar hoja" (igual que el bloqueo actual de `refsApproved`).

### Paso 3 — ESCENAS / CLIPS
Mismo contrato que comercial §Paso 3; `notas_editor` usa los campos del .md
inmobiliario: `{voz_en_off, musica, orden_sugerido}` (sin `eslogan` separado;
el gancho va dentro de `voz_en_off`). Continuidad: misma hora/paleta en todos
los clips del ángulo.

---

## 5. FLUJO CINE (paso a paso)

### Paso 0 — GUION
Guion, escena o brief narrativo + imágenes de personajes/locaciones/atmósfera +
`personajes_fijos` (referencia canónica por personaje). `registro` default `cine`.

### Paso 1 — ANÁLISIS DEL GUION (`artifact: scene_analysis`)
- **System**: REGLAS COMUNES + MÓDULO DE ACTUACIÓN + PASO 1 (agente-cine.md).
- **Entrada**: `{nombre_proyecto, contexto, cantidad_prompts, registro, referencias_subidas[], personajes_fijos[]}`.
- **Skills**: `get_project_brief`, `save_artifact`, `ask_user`.
- **Salida**: el contrato del .md — clave por la `biblia_continuidad`
  `{personajes[{descripcion, referencia}], locaciones[], hora_del_dia, paleta, tono}` y
  los `beats` con `linea_original`, `subtexto_objetivo`, `verbo`, `accion_fisica`,
  `microgesto`, `dialogo`, `importancia`.
- **Validación**: cantidad exacta de prompts; si una metáfora admite lecturas
  opuestas el system prompt obliga a `necesita_informacion` (regla 3 del .md);
  `personajes_fijos[]` cubre a todos los personajes citados en los beats.
- **UI**: escenas seleccionables; la biblia se muestra como panel de
  "Continuidad" (fijo en el wizard, visible en todos los pasos).

### Paso 2 — STORYBOARD
Igual que comercial §Paso 2 + diferencias cine:
- La viñeta incluye `miradas` (quién mira a quién) y la "lectura del espacio"
  (`lectura_del_espacio`) se muestra sobre las viñetas.
- La `biblia_continuidad` viaja en la entrada: hora/paleta/vestuario se
  mantienen entre viñetas y entre escenas continuas.
- `generate_image` de la hoja recibe además `reference_image_ids` de locación
  y personajes (los `@image` del contrato).

### Paso 3 — ESCENAS / CLIPS
Igual que comercial §Paso 3 + diferencias cine:
- Entrada extra: `continua_de: "@video1 | null"`. Si encadena un clip anterior,
  el artefacto debe abrir con "Continúa desde @videoN" y el front agrega el
  video anterior a `reference_image_ids` (regla: cada 3 clips encadenados se
  vuelve a incluir la imagen original del personaje).
- `notas_editor`: `{musica, sonido_sugerido, orden_sugerido}`.
- Diálogo con `@audio` (timbre de voz) cuando el usuario subió audio.

---

## 6. VALIDACIONES Y CASOS BORDE (comunes)

1. **JSON sucio**: los parsers (`extractArtifact`) toleran fences markdown y
   texto alrededor (ya hecho para `extractAngles`; se generaliza).
2. **Cantidad y duración**: siempre las del contrato; discrepancia → el back
   pide rehacer el turno con una instrucción de corrección (un solo reintento)
   y si persiste devuelve `advertencias` al front en vez de fallar.
3. **Proveedor sin soporte de tools** (OpenRouter/Anthropic son los únicos
   directos): el paso de artefacto funciona igual (no usa tools), solo se
   pierde `save_artifact` automático → el back persiste el artefacto él mismo
   tras validar, sin depender del LLM (decisión: `save_artifact` es un
   hook del back post-validación, no una tool invocable por el modelo).
4. **Timeout**: artefacto con timeout de 60 s (frente al de 40 s del chat);
   el front ya aborta a 120 s.
5. **Aislamiento**: historial y `agency_flows` siempre por
   `(tenant, user, agent)`; un `conversation_id` repetido en otro agente no
   cruza datos (la clave actual ya es por tenant+user; se agrega `agent`).
6. **Modelo pequeño** ("funciona con cualquier modelo", regla 9 de los .md):
   los contratos se validan en back y el reintento de corrección compensa
   modelos débiles.

---

## 7. PLAN DE IMPLEMENTACIÓN (por fases, con archivos)

### Fase 1 — Back: prompts por paso + artefactos
- `internal/modules/agent/prompts.go`: `//go:embed` de los tres .md + parser
  de secciones (`REGLAS COMUNES`, `MÓDULO DE ACTUACIÓN`, `PASO 1|2|3`).
- `internal/modules/agent/personas.go`: `systemFor(agent, step, ctx)` reemplaza
  `ChatSystem/AnglesSystem` (se conservan como fallback de chat libre).
- `internal/modules/agent/direct.go`: generalizar `wantsAngles` →
  `artifactTurn(req.Artifact)` con `extractArtifact(kind)` + validadores por
  contrato (`validateScenePrompts`, `validateStoryboard`, `validateClipPrompt`,
  `validateSalesAnglesExtendido`); eventos `artifact` y `questions`.
- Tests: `direct_test.go` (parsers + validadores), `personas_test.go` (secciones presentes).

### Fase 2 — Back: skills por agente/paso + persistencia
- `internal/toolkit`: `GetFlow/SaveFlow` + `agency_flows` (migración SQL nueva
  en el mismo esquema de seeds); interface `FlowStore` testeable con fake.
- `internal/modules/agent/tools.go`: `toolsFor(agent, step)`; nuevas skills
  `get_project_brief` y `ask_user`; `save_artifact` como hook post-validación.
- `main.go`: cablear el store de flows al módulo agent.

### Fase 3 — Front: wizard conectado a los artefactos
- `agency.component.ts`: sustituir `buildAngles`/plantillas de storyboard por
  los artefactos del paso (`scene_prompts`, `storyboard`, `clip_prompt`);
  manejar `artifact` y `questions`; un `conversationId` **por agente** (reset
  al cambiar de agente en el selector); persistir el flujo vía `agency_flows`.
- Retirar `agency-real-estate.component` (eventos legacy) tras migrar su UI al
  wizard; i18n ES/EN de los textos nuevos.

### Fase 4 — Personalización y MCP
- `internal/modules/skill`: resolver override `agent.<agente>.paso<n>` antes de
  la sección embebida (§2.5).
- `internal/modules/mcp`: exponer `get_flow`/`save_flow` como tools del servidor
  MCP remoto (mismo toolkit).

### Estimación de toques por fase
| Fase | Back | Front | Tests |
|---|---|---|---|
| 1 | prompts.go, direct.go, personas.go | — | parsers/validadores |
| 2 | toolkit, tools.go, main.go, migración | — | flow store + toolsFor |
| 3 | — | agency.component, i18n | spec del SSE |
| 4 | skill, mcp | — | overrides |

---

## 8. DECISIONES ABIERTAS (para cerrar antes de Fase 1)

1. **Mensajería del paso**: ¿el `message` del request lleva el payload del paso
   como JSON embebido, o se agrega un campo `payload` objeto al `chatRequest`?
   (Recomendado: campo `payload` — evita que el LLM tenga que parsear el brief
   del propio turno.)
2. **Un clip por viñeta o por segmento**: hoy el front arma prompts por
   segmento de duración del modelo. Recomendado: mantener segmentos (el modelo
   define el máximo) y validar que la suma cubra `duracion_total_s`.
3. **Retención de `agency_flows`**: ¿borrado lógico al archivar el proyecto?
4. **`agency-real-estate.component`**: confirmar retiro total vs. mantenerlo
   como vista "solo chat" del agente inmobiliario.
