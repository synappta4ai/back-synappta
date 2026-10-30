# Analizador de video (paso 1 de VideoMachine / Infoproducto)

`POST /api/v1/infoproduct/analyze` desglosa el video de referencia con un modelo
multimodal. Por defecto usa como **system prompt** el skill embebido
`Synappta Skill analizador de video.md` (`name: DCS-ANALYZE`, ~62 KB): devuelve
un único JSON con cortes, encuadres, movimientos de cámara, estilo de montaje,
ficha técnica, estilo visual, efectos, gancho y blueprint de recreación, más un
bloque `legacy_ui` con las secciones que muestra la pantalla "Desglose del video".

La copia embebida vive en `internal/modules/infoproduct/prompts/` y es idéntica
al original de `internal/Prompt/` (mismo nombre; el frontmatter se quita al
cargar). El breakdown guardado en `video_analyses.breakdown` mantiene los
nombres de campo que ya consume el front y añade el JSON completo en `dcs_analyze`.

Variables: `VIDEO_ANALYZER_SKILL` (`dcs-analyze` por defecto; `legacy` vuelve al
analizador anterior de `prompts/analyze.md`), `LLM_MODEL_ANALYZE_VIDEO` (modelo
solo para este paso, p. ej. `google/gemini-3.8-flash`) y `VIDEO_ANALYZE_FPS`
(1-24, informativo: el transporte OpenRouter entrega siempre 1 fps). Si el skill
no carga, el JSON no parsea o Gemini falla, se cae al analizador anterior y se
registra un warning. El paso 2 (skill DCS-RECREATE) consume esta ficha técnica
(ver la sección del paso 2 al final).

Con 1 fps el modelo no ve los cortes de menos de un segundo, así que la línea
de tiempo la decide ffmpeg: tras la respuesta, `reconcileTimeline` (reconcile.go)
parte `shots[]` en cada corte medido, hereda encuadre/movimiento/descripción del
plano que lo contiene y recalcula métricas y `legacy_ui.tomas`. Queda marcado en
`quality` (`timeline_source: "ffmpeg_measured"`, `reconciled`, `model_shots`,
`model_shots_list` con la lista original del modelo).

## Paso 2 (DCS-RECREATE) y la ficha técnica

El paso 2 recibe el desglose ya filtrado (`sanitizeBreakdown`, `recreate.go`): narración
legacy (`summary`, `pacing`, `setting_and_lighting`) más un bloque `technical` con
`visual_style`, `camera_summary`, `technical_signature`, `editing_style`,
`transitions_summary`, `effects_summary`, `recreation_blueprint`, `setting`,
`hook_and_structure` y los `shots[]` con su ficha forense (lente, profundidad de campo,
luz, exposición/color, efectos, transición). Se excluyen a propósito `talent`,
`brand_elements`, `graphics_summary`, `audio_observations` y
`legacy_ui.modelo_persona/musica`: el skill de recreación los prohíbe en el prompt.

El skill `Synappta Skill actuacion Infoproducto.md` (copia embebida en
`internal/modules/infoproduct/prompts/`) usa ese bloque: cada toma se escribe con las
líneas `Lens / Light / Colour / FX / Frame / Action / Cut`, más una línea `Edit curve` y el
`Look` derivados de la ficha técnica. Sin `technical` (análisis viejos) el paso 2 sigue con
el formato anterior.
