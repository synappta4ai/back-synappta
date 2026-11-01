# Agente Writing Copilot (VideoMachine)

Agente de VideoMachine que convierte una idea, un guion publicitario, un texto de
contexto o una historia corta en prompts de video con lenguaje cinematográfico
(planos, movimientos, lentes, luz, actuación, diálogo, audio y transiciones) y
duración por plano.

- **System prompt**: skill embebido `Synappta Skill writing copilot.md`
  (`name: Synappta-WRITE`), copia idéntica del original de `internal/Prompt/`.
  El frontmatter se quita al cargar y se valida el nombre; si no coincide, el
  agente queda deshabilitado (no se usa otro prompt como reemplazo).
- **Endpoints**: `GET /api/v1/writing-copilot/availability` y
  `POST /api/v1/writing-copilot/chat` (`message`, `history`, `params`,
  `references` opcional). La respuesta trae `text` y `prompts[]` con el
  contenido de cada bloque de código, para los botones de copiar.
- **Historial**: stateless; el front reenvía los turnos `user`/`assistant` que
  quiere que el cerebro considere.
- **Variables**: `WRITING_COPILOT_ENABLED` (`true`/`false`),
  `LLM_MODEL_WRITING_COPILOT` (vacío = default de texto del proyecto) y
  `WRITING_COPILOT_TEMPERATURE` (0-2, default 0.7).
- **Desactivar**: `WRITING_COPILOT_ENABLED=false` (el agente desaparece del
  selector y el endpoint responde 503) o dejar el skill con otro `name`.
- **Costo**: cada turno envía el skill completo (~51 KB) como system prompt.

**Modelo del copiloto**: el front elige un modelo de texto del catálogo
(openrouter/... usa la API key de OpenRouter del tenant; nthropic/... usa la
API key directa de Anthropic). LLM_MODEL_WRITING_COPILOT gana sobre la elección
del usuario. Si el proveedor elegido no tiene key, se cae al otro si tiene.

**Parámetros**: además de los del skill (output_language, delivery,
spect_ratio, 	otal_duration_s, max_clip_s, ps, look, udio_mode,
	arget_model, esponse_mode) se envía shot_count (cortes por clip), que el
skill acepta como parámetro declarado por el usuario.

**Paso final**: el botón "Generar prompt final" pide output_language=en,
esponse_mode=prompt_only y un manifiesto eferences con los insumos subidos
(imágenes con rol, video y audio). El front pinta los tags con el sistema de
colores (prompt-tone-*).