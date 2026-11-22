# Looks (esquemas de luz y color) - Documento completo

> Antes: "Plantillas de iluminación". En la app se muestra como **Look** (chip en Fábrica de videos) y **Look de luz** (modal y Estudio).
> Incluye los DOS sets del documento original: **SET 1 (formato largo, 31 looks)** y **SET 2 (formato corto, 31 looks)**.

## Qué cambió respecto al original

1. De "copiar exacto" a **adaptar**: el look se interpreta a través de la escena de entrada, sin copiarla.
2. Set 1: cada look se divide en capa **TRANSFERABLE** (siempre aplica) y capa **CONDICIONAL** (solo si es físicamente plausible en la escena).
3. Set 2: cada look conserva su descripción original y añade una línea de **GUARD** cuando el look puede provocar objetos o patrones inventados (persianas, ventanas, hojas, lámparas, pantallas, neón).
4. Se eliminó el lenguaje absoluto: "match it exactly", "same ... as", "must disappear completely", "becomes".
5. Se añadió `{INTENSITY_BLOCK}` (Sutil / Media / Fuerte) y la cláusula `SCENE FIDELITY`.
6. Se quitaron referencias al video de prueba (gorra, sofá, micrófono de pecho, cuarto beige). Todo es agnóstico a la escena, válido para video e imagen.
7. Looks sin captura original, redactados por nosotros y marcados `[BORRADOR]`: Set 1 -> 01 Clamshell, 31 Bloom/halation. Set 2 -> 11 Rim/Backlight, 14 Bounce, 30 Monochrome.

## Variables

| Variable | Qué reemplaza la app |
|---|---|
| `{LOOK_NAME}` | Nombre del look elegido |
| `{LOOK_BLOCK}` | Bloque del look (Set 1: capas TRANSFERABLE + CONDICIONAL. Set 2: descripción + GUARD) |
| `{INTENSITY_BLOCK}` | Uno de los 3 bloques de intensidad |
| `{SOURCE}` | `@video1` (video) o `@imagen1` (imagen de entrada) |
| `{REF}` | Imagen de referencia del look si el usuario la sube (opcional; si no hay, eliminar la línea) |

## Cuándo usar cada set

- **Set 1 (largo):** relight completo, máxima fidelidad del look con bloques detallados (atmósfera, paleta, negros, patina).
- **Set 2 (corto):** cambio rápido de luz y color, se apoya en una referencia `{REF}` solo para luz y color.
- Ambos sets son válidos para video e imagen. En imagen, ignora las instrucciones de tiempo (flicker, movimiento, voz).

## Intensidad (aplica a ambos sets)

**Sutil (30-40%)**
```
INTENSITY: SUBTLE. Grade only: shift color temperature, palette, contrast, black level and highlight tone toward the look. Do NOT add new light sources, light patterns or atmospheric effects. The scene must remain clearly the same scene with a refined grade.
```

**Media (60%, por defecto)**
```
INTENSITY: MEDIUM. Grade plus relight: reinforce and recolor the light sources that already exist, shift direction and quality of light toward the look, and add the look's atmosphere only where it makes physical sense. Original colors and materials stay recognizable.
```

**Fuerte (90%)**
```
INTENSITY: STRONG. Full relight and regrade in the spirit of the look: dominant palette, strong contrast and atmosphere. Still never invent objects, architecture or shadow patterns that cannot exist in this location, and never copy specific shapes from the look's reference.
```

---

# SET 1 - Formato largo (31)

## Plantilla base - Video

```
CINEMATIC CAMERA SETTINGS: exactly the camera, lens, focal length, framing and aspect ratio of {SOURCE}; natural motion blur; film grain and lens rendering consistent with the look.

Create a cinematic video in one continuous shot, the same length as {SOURCE}.

REFERENCES:
{SOURCE} = the master for camera, framing, perspective, location layout, subject, face, outfit, performance, timing and voice - all preserved exactly.
{REF} (only if provided) = a visual hint of the look ({LOOK_NAME}): mood, tone, palette, shadows, blacks, highlights and patina. NEVER a start frame, NEVER a layout: its framing, composition, location, people, faces, clothes and objects are NEVER used.

MASTER LOCK - HIGHEST PRIORITY: {SOURCE} is the master. Camera, angle, height, lens, perspective, framing, distance, subject size, every camera movement, location layout, every object, and the subject's position, movement, gesture, gaze, blink, expression, lip movement, acting and timing stay identical frame by frame. The subject's FACE stays exactly the face in {SOURCE}, only re-lit. Outfit and hair stay as in {SOURCE}. Preserve the original VOICE and speech exactly. Never add, remove, change, anticipate or delay any action or camera move. One continuous shot, no cuts.

LOOK ADAPTATION - HIGHEST PRIORITY: re-grade and re-light {SOURCE} so it feels like the look "{LOOK_NAME}", keeping its own architecture, objects, materials and color identity recognizable. Translate the look into the scene: keep the quality of light, color temperature, contrast, black level and mood, but ONLY create light sources, shadows, patterns and effects that make physical sense in this location. If an element of the look has no plausible source in the scene (blinds, windows, leaves, tubes, bulbs, beams, flares), do NOT invent it: reproduce its color and mood with the sources that already exist. Real light that wraps the subject and reacts to every gesture - never a flat filter, never a color overlay. Eyes stay natural, never glowing.

{INTENSITY_BLOCK}

{LOOK_BLOCK}

SCENE FIDELITY: do not add objects, windows, blinds, lamps, bulbs, tubes, foliage or architecture that are not in {SOURCE}. Any new light must come from sources already present or implied off-screen. Shadow shapes on floors, walls or faces must come from real objects in the scene.

THE ONLY CHANGES from {SOURCE} are the light and color grade of the look. Everything else is preserved.

Not a 3D render, no digital sharpness, no AI smoothing.

AUDIO: keep the original voice and sounds of {SOURCE} untouched. No added sound effects. NO music, NO score.
```

## Plantilla base - Imagen

```
Re-light and re-grade {SOURCE} with the look "{LOOK_NAME}".

PRESERVE EXACTLY: composition, framing, aspect ratio, camera angle, lens, perspective, location, every object, subject identity, face, expression, pose, outfit and hair. Only the light and the color grade change.

LOOK ADAPTATION - HIGHEST PRIORITY: interpret the look through the actual scene. Keep its own architecture, objects, materials and color identity recognizable. Keep the quality of light, color temperature, contrast, black level and mood of the look, but ONLY create light sources, shadows, patterns and effects that make physical sense in this location. If an element of the look has no plausible source in the scene, do NOT invent it: reproduce its color and mood with the sources that already exist. Real light that wraps the subject - never a flat filter, never a color overlay. Eyes stay natural, never glowing.

{INTENSITY_BLOCK}

{LOOK_BLOCK}

SCENE FIDELITY: do not add objects, windows, blinds, lamps, bulbs, tubes, foliage or architecture that are not in {SOURCE}. Any new light must come from sources already present or implied off-screen. Shadow shapes must come from real objects in the scene.

Photographic result, natural skin texture, fine film grain. Not a 3D render, no digital sharpness, no AI smoothing.
```

## Looks del Set 1

### 01. Clamshell [BORRADOR]

```
LOOK - CLAMSHELL. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Bright, soft, flattering beauty look, clean and intimate.
- COLOR PALETTE: Natural clean skin tones, soft neutral whites, muted background colors.
- SHADOWS AND BLACKS: Gently lifted soft blacks, no crushed shadows.
- HIGHLIGHTS: Soft creamy skin highlights, never clipped.
- CONTRAST AND SATURATION: Low-medium contrast, natural saturation.
- PATINA: Fine natural grain, clean polished beauty finish.
- LIGHT QUALITY AND DIRECTION: Two large soft sources in a clamshell: a soft key from above the lens and a soft fill from below, wrapping the face evenly, small catchlights, minimal shadows under the chin and eyes.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- The surroundings stay soft and slightly darker than the face, gently falling off behind the subject, no hard background light. Never show the lights.
```

### 02. Three-point

```
LOOK - THREE-POINT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Low, warm and intimate evening interior; the face is NOT brighter than the room.
- COLOR PALETTE: Amber, warm beige, olive green, brown, muted cream, golden-bronze skin.
- SHADOWS AND BLACKS: Deep brown-black blacks, olive-brown shadows.
- HIGHLIGHTS: Soft warm sand-beige, never white, never clipped.
- CONTRAST AND SATURATION: Gentle low-medium contrast, muted natural saturation.
- PATINA: Soft creamy film texture, fine grain, gentle bloom on warm practicals.
- LIGHT QUALITY AND DIRECTION: A soft warm tungsten-like key from one side in front of the subject: that side of the face lit, the other side one stop darker in a soft gradual shadow; no rim light, no back light.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Background shifts toward a dim warm beige-amber that glows slightly brighter than the face, soft and defocused, like a cafe at evening; warm practicals only if the location can host them, otherwise warm the existing sources.
```

### 03. Cross lighting

```
LOOK - CROSS LIGHTING. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Very dark, low-key, claustrophobic nightclub mood, about three stops under the original.
- COLOR PALETTE: Deep bottle green, olive, black, touches of magenta-red and orange.
- SHADOWS AND BLACKS: Crushed pure black outside the light.
- HIGHLIGHTS: Dim green-yellow, never bright, never white.
- CONTRAST AND SATURATION: High contrast, high saturation.
- PATINA: Heavy grain, murky soft image, smoky darkness.
- LIGHT QUALITY AND DIRECTION: Two opposed colored lights: a dim hard green from one side and a red-magenta from the other touching the edge of hair and cheek, with a dark shadow line down the middle of the face.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Walls and objects sink toward black; blurry dark green and red-magenta light shapes in the background only if there is depth or space for them.
```

### 04. Kicker

```
LOOK - KICKER. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Medium-low key, warm and intimate with a cool contrast behind.
- COLOR PALETTE: Warm gold skin, olive brown, teal-green, mint-white.
- SHADOWS AND BLACKS: Deep near-black with a green tint.
- HIGHLIGHTS: Warm golden on the skin edge, mint-white on the cool glow.
- CONTRAST AND SATURATION: High contrast, medium saturation.
- PATINA: Rich 35mm film, fine grain, slight halation around bright cool areas.
- LIGHT QUALITY AND DIRECTION: A warm golden kicker from behind and to one side skimming cheek, jaw, ear, neck and shoulder with a hot edge; the front of the face softer and warm.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surroundings lean dim olive-brown with deep teal-green shadows. A cool mint-white glow behind the subject ONLY if there is a window, opening or bright area; otherwise use a cool ambient glow or none.
```

### 05. Eye light

```
LOOK - EYE LIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark, hot and claustrophobic.
- COLOR PALETTE: Rust red, brown, amber, olive-yellow.
- SHADOWS AND BLACKS: Crushed red-black.
- HIGHLIGHTS: Warm amber-yellow with small wet glints, never white.
- CONTRAST AND SATURATION: High contrast, high saturation.
- PATINA: Gritty 1990s 35mm film, coarse grain, a sickly warm cast.
- LIGHT QUALITY AND DIRECTION: A narrow horizontal band of warm light only across the eyes, following every head movement (video); the rest of the face in a dim red-orange shadow. Eyes stay natural, never glowing.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surroundings sink into dark red-brown darkness, background almost invisible. If the subject is not a person, use a narrow warm band over the focal area.
```

### 06. Ring light

```
LOOK - RING LIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Low-key night interior, dim overall, face moderately exposed, warm intimate dressing-room mood.
- COLOR PALETTE: Warm amber-orange, dusty mauve, lavender, baby blue, hot-pink accents, brown.
- SHADOWS AND BLACKS: Lifted dark brown-mauve, never pure black.
- HIGHLIGHTS: Warm orange-cream, soft roll-off.
- CONTRAST AND SATURATION: Medium contrast, medium saturation.
- PATINA: Soft image, slight haze, gentle bloom on practicals.
- LIGHT QUALITY AND DIRECTION: A soft warm frontal light from the camera position, flat on the face, warm amber-orange, golden-brown skin. The ring light is never visible. Eyes stay natural.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Background tints toward dusty mauve and blue-grey; soft pink, lavender and baby-blue practical bokeh only if the scene has lights or depth to support them.
```

### 07. Direct flash

```
LOOK - DIRECT FLASH. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Night, raw paparazzi snapshot: subject bright, background dark.
- COLOR PALETTE: Black, dark bottle green, warm skin, pale cream-yellow on flashed surfaces.
- SHADOWS AND BLACKS: Crushed green-black.
- HIGHLIGHTS: Warm peach-cream, slightly blown.
- CONTRAST AND SATURATION: Very high contrast, medium saturation.
- PATINA: 35mm point-and-shoot film, heavy grain, slight vignette, warm-green tint.
- LIGHT QUALITY AND DIRECTION: On-camera flash from the lens position: hard flat frontal light, fast falloff, constant in every frame without flicker.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surfaces close to the subject catch the flash and turn pale cream-yellow; the far background falls to black. A hard shadow of the subject on the wall ONLY if a wall is close behind.
```

### 08. Harsh midday sun

```
LOOK - HARSH MIDDAY SUN. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Bright, hard summer daylight, restrained grade.
- COLOR PALETTE: Neutral greys, concrete grey, slate green-grey, muted brick.
- SHADOWS AND BLACKS: Slightly lifted grey.
- HIGHLIGHTS: Neutral white close to clipping.
- CONTRAST AND SATURATION: High contrast between sun and shade, low saturation.
- PATINA: Clean natural 35mm, fine grain.
- LIGHT QUALITY AND DIRECTION: Hard overhead sun: short hard shadows under brims, noses and chins; areas outside direct sun fall into neutral grey shade.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Crisp sunlit patches only on surfaces actually exposed to sun. Dappled leaf shadows ONLY if there is foliage; NEVER invent blinds, window grids or foliage shadows.
```

### 09. Snow light

```
LOOK - SNOW LIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Bright, cold daylight, crisp.
- COLOR PALETTE: Cold white, steel blue, teal-grey, deep black, muted brown.
- SHADOWS AND BLACKS: Deep blue-black.
- HIGHLIGHTS: Cold mint-white.
- CONTRAST AND SATURATION: Medium-high contrast, low-medium cool saturation.
- PATINA: Crisp image, fine grain.
- LIGHT QUALITY AND DIRECTION: Soft overcast frontal light plus a strong white bounce from below, filling eye sockets, chin and any brim shadows.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surfaces lean cool blue-grey and reflect white light from below; existing materials stay recognizable.
```

### 10. Lens flare

```
LOOK - LENS FLARE. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Golden hour, warm, backlit and dreamy.
- COLOR PALETTE: Gold, amber, honey, warm brown, cream white.
- SHADOWS AND BLACKS: Lifted warm brown haze, no true black.
- HIGHLIGHTS: Pure white blooming highlights.
- CONTRAST AND SATURATION: Low contrast from veiling glare, warm medium saturation.
- PATINA: Golden haze, strong halation, flare ghosts, fine grain.
- LIGHT QUALITY AND DIRECTION: A low strong sun behind the subject toward the lens: golden rim on hair and edges, soft flare streaks and veiling glare, the face in warm bounce.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Flare streaks and ghosts move naturally with the camera; the background becomes a glowing golden haze only where there is open space or sky behind.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 11. Lightning

```
LOOK - LIGHTNING. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark stormy night, tense.
- COLOR PALETTE: Deep teal, navy, cyan, cold white, faint violet.
- SHADOWS AND BLACKS: Teal-black.
- HIGHLIGHTS: Blue-white, clipped during the flashes, with a glow.
- CONTRAST AND SATURATION: High contrast, medium-high cool saturation.
- PATINA: Crisp cinematic image, fine grain.
- LIGHT QUALITY AND DIRECTION: Cold dim moonlight as base; at irregular moments (video) or as one frozen flash (still) a blue-white lightning blast hits from one side.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surroundings go dark teal-blue night; existing objects stay visible as dark blue-grey shapes.
```

### 12. Atmospheric fog

```
LOOK - ATMOSPHERIC FOG. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Cold, misty, quiet.
- COLOR PALETTE: Grey, taupe, warm grey-white, desaturated olive.
- SHADOWS AND BLACKS: Lifted grey, no true black.
- HIGHLIGHTS: Warm grey-white.
- CONTRAST AND SATURATION: Very low contrast, very low saturation.
- PATINA: Volumetric rays, moving haze, soft image.
- LIGHT QUALITY AND DIRECTION: Soft daylight backlight through haze with a soft frontal fill; subject partly silhouetted; everything desaturated.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Volumetric rays and a misty back glow wherever there is depth behind the subject.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 13. Sodium vapour

```
LOOK - SODIUM VAPOUR. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Very dark gritty night.
- COLOR PALETTE: Orange, amber, dark red-brown, black; dominated by orange.
- SHADOWS AND BLACKS: Pure black.
- HIGHLIGHTS: Amber-yellow, small glows around light sources.
- CONTRAST AND SATURATION: Very high contrast, high warm saturation.
- PATINA: Gritty grain, deep blacks.
- LIGHT QUALITY AND DIRECTION: An orange sodium streetlight from above and to one side as the dominant light source.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surfaces outside the orange light fall to black; a visible lamp only if it fits the location, otherwise keep it off-screen.
```

### 14. Fluorescent

```
LOOK - FLUORESCENT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Flat, tired, institutional.
- COLOR PALETTE: Warm grey, beige, teal-navy, cream-white.
- SHADOWS AND BLACKS: Neutral dark grey.
- HIGHLIGHTS: Blown warm white.
- CONTRAST AND SATURATION: Medium contrast, low-medium saturation.
- PATINA: Clean digital cinema, shallow depth of field, soft bokeh.
- LIGHT QUALITY AND DIRECTION: Flat top light from above, shadows in eye sockets and under brims, slight sheen on the skin.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Out-of-focus long bright tube shapes in the background ONLY if the location has overhead fixtures; otherwise keep only the flat greenish-white top light.
```

### 15. Bare bulb

```
LOOK - BARE BULB. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Low-key, gritty, sweaty basement.
- COLOR PALETTE: Amber, brown, ochre, olive, small cold blue-grey accents.
- SHADOWS AND BLACKS: Crushed.
- HIGHLIGHTS: Warm amber.
- CONTRAST AND SATURATION: High contrast, high warm saturation.
- PATINA: 1990s 35mm film, grain.
- LIGHT QUALITY AND DIRECTION: Hard warm top light from a single bulb just above the frame: hot highlights on head and shoulders, deep shadow under brims, fast falloff, sweaty sheen.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Bulb glow at the top edge only if framing allows; surroundings lean grimy brown-olive and amber.
```

### 16. Flashlight

```
LOOK - FLASHLIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dim, cold, clinical.
- COLOR PALETTE: Teal, cyan-grey, muted mint, warm orange-white from the torch.
- SHADOWS AND BLACKS: Lifted blue-grey.
- HIGHLIGHTS: Mint-white, the source hot warm-white.
- CONTRAST AND SATURATION: Medium-low contrast, medium saturation.
- PATINA: Soft image, shallow depth of field, glow.
- LIGHT QUALITY AND DIRECTION: A flashlight beam from near the camera: a hard pool of light on the subject with sharp falloff, slightly wobbling in video.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Background in dim teal-grey with soft round pale bokeh where there is depth.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 17. Dashboard light

```
LOOK - DASHBOARD LIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Night, hypnotic, close to monochrome cyan.
- COLOR PALETTE: Electric cyan, turquoise, deep navy, black.
- SHADOWS AND BLACKS: Black-blue.
- HIGHLIGHTS: Electric cyan.
- CONTRAST AND SATURATION: Medium contrast, extreme saturation.
- PATINA: Smoky haze, soft glow.
- LIGHT QUALITY AND DIRECTION: Soft cyan glow from below and in front, lighting the underside of chin, nose and brims.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Cyan pools on nearby surfaces; the rest deepens to blue darkness while its own colors stay faintly readable. Do NOT paint the whole scene one flat color.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 18. Police lights

```
LOOK - POLICE LIGHTS. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Almost total darkness, tense night.
- COLOR PALETTE: Black, deep brown, magenta-red, violet-blue.
- SHADOWS AND BLACKS: Pure black.
- HIGHLIGHTS: Small magenta and blue highlights.
- CONTRAST AND SATURATION: Extremely low key, high saturation in the lights.
- PATINA: Grain, sweaty sheen, dotted bokeh.
- LIGHT QUALITY AND DIRECTION: Dim warm red-brown base light on the subject with alternating red and blue sweeps in a steady rhythm (video) or a red/blue split (still).

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Blurred red and blue glows at frame edges as if through windows, only if the location can plausibly have them.
```

### 19. String lights

```
LOOK - STRING LIGHTS. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Warm festive night.
- COLOR PALETTE: Warm cream, golden, olive green, brown, dark green.
- SHADOWS AND BLACKS: Olive-brown.
- HIGHLIGHTS: Warm cream.
- CONTRAST AND SATURATION: Medium contrast, medium saturation.
- PATINA: Warm soft image, slight haze, glow around bulbs.
- LIGHT QUALITY AND DIRECTION: Soft warm light on the subject from many small warm sources.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Out-of-focus string bulbs in the background ONLY if the scene is outdoors or has space for them; otherwise use scattered warm practicals.
```

### 20. Projector beam

```
LOOK - PROJECTOR BEAM. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark, awe, cinema.
- COLOR PALETTE: Dark teal, olive, white-yellow, cyan, orange, pink.
- SHADOWS AND BLACKS: Dark teal.
- HIGHLIGHTS: White.
- CONTRAST AND SATURATION: High contrast, high saturation in the beams.
- PATINA: Haze, glow, flare.
- LIGHT QUALITY AND DIRECTION: Face dim and warm-neutral from the front, flickering slightly with the projected image (video).

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Fanning colored beams from behind toward the lens, only if there is open depth behind the subject.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 21. Stained glass

```
LOOK - STAINED GLASS. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark, sacred, contemplative.
- COLOR PALETTE: Navy, deep blue, cyan, pale gold, red sparks.
- SHADOWS AND BLACKS: Dark navy.
- HIGHLIGHTS: Cyan-white glowing windows.
- CONTRAST AND SATURATION: High contrast, medium-high saturation.
- PATINA: Haze, soft image.
- LIGHT QUALITY AND DIRECTION: Subject partly silhouetted, face in soft shadow with a cool rim from behind and a soft top beam.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Jewel-colored bokeh windows in the background ONLY if the architecture allows; otherwise use jewel-toned color accents in the light.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 22. Searchlight

```
LOOK - SEARCHLIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Black night, tension.
- COLOR PALETTE: Black, navy, pale cyan-white.
- SHADOWS AND BLACKS: Pure black.
- HIGHLIGHTS: Pale cyan-white beam.
- CONTRAST AND SATURATION: Extreme contrast, low saturation.
- PATINA: Haze, fine grain.
- LIGHT QUALITY AND DIRECTION: One hard searchlight beam from high above cutting diagonally through haze, sweeping slowly (video); the subject almost a silhouette with an edge of light.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Scene falls toward black; silhouettes of the main elements stay readable.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 23. Explosion

```
LOOK - EXPLOSION. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dramatic, chaotic.
- COLOR PALETTE: Teal, cyan, orange, yellow, black.
- SHADOWS AND BLACKS: Teal-black.
- HIGHLIGHTS: Yellow-orange fire, cyan-white backlight.
- CONTRAST AND SATURATION: High contrast, high saturation.
- PATINA: Smoke, haze, volumetric rays, glow.
- LIGHT QUALITY AND DIRECTION: Subject almost silhouetted with a hot rim; fire light flickers and pulses from one side; cool teal-cyan backlight from the other.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Smoke and volumetric rays in available depth. Light only: no visible fire, debris or explosion unless the scene already has it.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 24. Muzzle flash

```
LOOK - MUZZLE FLASH. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Gritty, raw action.
- COLOR PALETTE: Mint green, olive, warm amber, brown, white.
- SHADOWS AND BLACKS: Warm brown-black.
- HIGHLIGHTS: Flashes blown to white.
- CONTRAST AND SATURATION: High contrast, high saturation.
- PATINA: 1990s 35mm film, grain, slight smear.
- LIGHT QUALITY AND DIRECTION: Warm dim light with split-second bursts of bright white-yellow from off-screen (video) or one frozen burst (still). Light only: no weapon, nothing visible.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Surfaces lean dirty mint and pale green; a warm tungsten glow at the top only if fixtures plausibly exist.
```

### 25. Laser lights

```
LOOK - LASER LIGHTS. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark, flat, mysterious.
- COLOR PALETTE: Navy, dark blue, red laser line, faint magenta.
- SHADOWS AND BLACKS: Lifted murky purple-grey.
- HIGHLIGHTS: Magenta-red, only on the line.
- CONTRAST AND SATURATION: Very low contrast, low saturation except the laser.
- PATINA: 16mm feel, heavy grain, soft murky image, slight vignette.
- LIGHT QUALITY AND DIRECTION: Dim blue ambient on the subject and a red glow wherever the laser touches the skin.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- A thin red laser line sweeping across the frame at eye and nose height; light only.

ATMOSPHERE RULE: haze, volumetric light and particles, if used, stay present and moving for the entire duration (video), never static. In stills, keep them subtle and consistent with the scene.
```

### 26. Blacklight (UV)

```
LOOK - BLACKLIGHT (UV). Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dark club, electric.
- COLOR PALETTE: Ultramarine, violet, neon orange, magenta, glowing white.
- SHADOWS AND BLACKS: Pure black.
- HIGHLIGHTS: Glowing whites and neon.
- CONTRAST AND SATURATION: High contrast, extreme saturation.
- PATINA: Glow, slight bloom.
- LIGHT QUALITY AND DIRECTION: Ultraviolet blacklight: skin purple-blue, light or white materials glow brightly.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Only existing white or light-colored materials glow; do not add neon paint, prints or signs that are not there.
```

### 27. Bi-color light

```
LOOK - BI-COLOR LIGHT. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Night party, cool and seductive.
- COLOR PALETTE: Royal blue, cyan, magenta-red, orange specks.
- SHADOWS AND BLACKS: Dark navy.
- HIGHLIGHTS: Blue-lavender, magenta-red on the skin.
- CONTRAST AND SATURATION: Medium-high contrast, high saturation.
- PATINA: Shallow depth of field, sparkly bokeh.
- LIGHT QUALITY AND DIRECTION: Magenta-red key from one side, deep blue light from the other and on the background.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Glittering blue, cyan and orange bokeh sparkles only where there is background depth or lights.
```

### 28. Mixed colour temperature

```
LOOK - MIXED COLOUR TEMPERATURE. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Warm afternoon, stylish.
- COLOR PALETTE: Orange, rust, golden yellow, teal, deep green.
- SHADOWS AND BLACKS: Reddish brown.
- HIGHLIGHTS: Warm cream-yellow.
- CONTRAST AND SATURATION: High contrast, high saturation.
- PATINA: Rich warm 35mm.
- LIGHT QUALITY AND DIRECTION: Warm golden sunlight on one side of the subject with a cool teal fill on the other.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Warm sun through curtains or a hard sun streak on the floor ONLY if there is a window or opening; otherwise keep the warm-versus-cool split in the light itself.
```

### 29. Cast shadow

```
LOOK - CAST SHADOW. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Warm, expressionist.
- COLOR PALETTE: Golden amber, honey, burnt orange, dark brown, black.
- SHADOWS AND BLACKS: Near-black warm.
- HIGHLIGHTS: Golden peach.
- CONTRAST AND SATURATION: High contrast, high warm saturation.
- PATINA: Warm grain.
- LIGHT QUALITY AND DIRECTION: A single hard warm low light from one side, with the edges of the frame falling to dark brown-black.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- A large hard shadow of the subject on a back wall ONLY if a wall is behind; otherwise rely on face and body shadows.
```

### 30. City bokeh

```
LOOK - CITY BOKEH. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Cool dusk, melancholic.
- COLOR PALETTE: Slate blue, grey, muted teal, pink-red, white.
- SHADOWS AND BLACKS: Slightly lifted blue-grey.
- HIGHLIGHTS: Cool white.
- CONTRAST AND SATURATION: Medium contrast, low saturation.
- PATINA: Soft, natural, fine grain.
- LIGHT QUALITY AND DIRECTION: Soft cool dusk light on the subject.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Heavily defocused cool background with round pink, red and white bokeh lights if the scene has distant lights or depth.
```

### 31. Bloom / halation [BORRADOR]

```
LOOK - BLOOM / HALATION. Use this as a lighting DIRECTION, not a recipe. Adapt it to the actual scene.

TRANSFERABLE (always apply):
- MOOD AND EXPOSURE: Dreamy, glowing, soft and nostalgic.
- COLOR PALETTE: Warm cream, soft amber, gentle pastel tones, skin warm and glowing.
- SHADOWS AND BLACKS: Softly lifted blacks with a faint haze.
- HIGHLIGHTS: Strongly blooming whites with warm-red halation edges.
- CONTRAST AND SATURATION: Low-medium contrast, medium-low saturation.
- PATINA: Film halation, soft diffusion, fine grain.
- LIGHT QUALITY AND DIRECTION: Soft light on the face with a glowing halo around bright edges, a subtle warm-red halation fringe around highlights and hair.

CONDITIONAL (only if physically plausible in the scene; otherwise skip and approximate with existing sources):
- Every bright area blooms softly into its surroundings, a gentle luminous veil; halation appears only around bright sources or highlights already present.
```

---

# SET 2 - Formato corto (31)

## Plantilla base

```
LIGHTING & COLOUR CHANGE ONLY - {LOOK_NAME}.

REFERENCES: {SOURCE} is my original and the only source for the subject, scene, framing and action. {REF} (only if provided) is a reference for lighting, colour and light effects only. Do not copy its people, clothes, location, composition or shadow shapes, and never use it as a start frame.

PRESERVE MY SHOT: keep the same subjects, faces, hair, clothing, setting and objects. Preserve the camera movement, framing, duration, timing, expressions, gestures and lip sync (video) or the exact composition (image). Do not add, remove or reposition subjects or objects.

LIGHTING: relight my original scene in the spirit of the look "{LOOK_NAME}", adapting it to what is physically in the scene. Interpret the look: do not copy it literally. Only create light sources, shadows and patterns that make physical sense here; if an element of the look has no plausible source, reproduce its color and mood with the existing sources instead of inventing it.

{INTENSITY_BLOCK}

{LOOK_BLOCK}

Keep shadows, falloff and reflections physically believable and consistent as the subject and camera move.

FINISH: photorealistic, natural skin texture and the original lens perspective. Preserve the original speech and audio. No added music or text.
```

## Looks del Set 2

### 01. High key

```
LOOK - HIGH KEY: Bright, even, almost shadowless light from large soft frontal sources and strong fill. Low contrast, airy whites, clean bright skin. The existing setting becomes luminous, with minimal shadows under the chin and on surrounding surfaces, for a crisp commercial look.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 02. Low key

```
LOOK - LOW KEY: The existing setting falls mostly into deep shadow. One soft key light creates a pool of light on the subject's face and upper body. High contrast, moody near-black background, only faint shapes of the surroundings.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 03. Rembrandt

```
LOOK - REMBRANDT: One key light high and about 45 degrees to the side. The far cheek stays in shadow with a small bright triangle beneath the eye on that side. Little fill, rich moody contrast, painterly warm tones, a darker background.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 04. Butterfly (Paramount)

```
LOOK - BUTTERFLY (PARAMOUNT): The key light high and directly in front of the face. A small butterfly-shaped shadow beneath the nose, soft shadows under chin and cheekbones. Glamorous classic Hollywood portrait look, soft falloff behind the subject.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 05. Split

```
LOOK - SPLIT: A hard side light at 90 degrees illuminating about half of the subject's face. The other half falls into darkness with a sharp vertical division. Dramatic high contrast, dark surroundings.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 06. Loop

```
LOOK - LOOP: A soft key slightly off-axis and just above eye level. A small nose shadow looping toward the cheek, gentle fill, soft modelling across the face. Natural and flattering.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 07. Broad

```
LOOK - BROAD: Light the side of the face turned toward the camera. The broader visible side is bright, gentle shadow on the far side. Soft open feel, natural warm portrait tones.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 08. Short

```
LOOK - SHORT: Light the side of the face turned away from the camera. The nearer side falls into soft shadow. Sculpted, moodier portrait look with greater depth and a slightly darker tone in the surroundings.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 09. Chiaroscuro

```
LOOK - CHIAROSCURO: Painterly, very high contrast. Carve the subject's face and hands out of deep darkness with one strong directional light. Rich warm highlights and deep blacks, the surroundings nearly disappearing into shadow like an old-master painting.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 10. Silhouette

```
LOOK - SILHOUETTE: The background is lit brightly and evenly, almost white. The subject is unlit from the front so body and face read as a dark silhouette, with only a faint hint of facial detail. Preserve the original outline and movements.
GUARD: Only brighten the background using areas that already exist as background (sky, wall, window, open space). Do not add a window or backdrop.
```

### 11. Rim / Backlight [BORRADOR]

```
LOOK - RIM / BACKLIGHT: A light from behind the subject that traces a clean bright edge along hair, shoulders and contours, separating the subject from a darker background. The front of the face stays softer and in gentle shadow or soft fill.
GUARD: The backlight comes from a plausible off-screen or existing source behind the subject. Do not add visible lamps or windows.
```

### 12. Hard light

```
LOOK - HARD LIGHT: A small, direct, unfiltered light source. Crisp sharp-edged shadows from the subject onto nearby surfaces, high clarity, punchy contrast and strong directional modelling.
GUARD: Shadows come only from the subject and from objects that already exist.
```

### 13. Soft light

```
LOOK - SOFT LIGHT: A large diffused source that wraps around the subject. Shadows roll off very gently, low contrast, soft natural skin highlights. Skin texture stays intact and the setting is softly lit for a calm, elegant look.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 14. Bounce [BORRADOR]

```
LOOK - BOUNCE: Soft, indirect light bounced from a large nearby surface (white wall, ceiling or reflector) into the subject: wraparound fill, gentle shadows, soft warm-neutral catchlights, an open natural look.
GUARD: The bounce color follows the surface it would come from in the scene; do not add a visible reflector or panel.
```

### 15. Top light

```
LOOK - TOP LIGHT: A single light directly overhead. The top of the head or existing headwear and the shoulders brighten. Eye sockets and sides fall into shadow, strong shadows under nose and chin, an interrogation-room mood.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 16. Under lighting

```
LOOK - UNDER LIGHTING: Light from below pointing upward at the face. Shadows throw upward over the eyes and forehead. Warm uplight and long upward shadows on nearby surfaces for an eerie campfire-story or horror look.
GUARD: Upward shadows on background surfaces only where such surfaces exist.
```

### 17. Golden hour

```
LOOK - GOLDEN HOUR: Low, warm sunlight entering the scene from the side. Long soft shadows, golden skin, warm highlights in hair. A slight lens glow and a rich amber grade while preserving the original setting.
GUARD: If the scene is indoors, imply the sun as a warm directional light from an existing or off-screen opening.
```

### 18. Blue hour

```
LOOK - BLUE HOUR: Cool ambient dusk light, no hard sunlight. Deep soft blues in the surroundings, gentle cool daylight on the face. A calm melancholic mood and slightly reduced exposure.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 19. Day-for-night

```
LOOK - DAY-FOR-NIGHT: Grade the scene deep blue and lower the exposure to suggest night. Cool moonlit tones, a darkened background, deep shadows, desaturated cooler skin. Keep enough detail to read the subject and the original action.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 20. Moonlight

```
LOOK - MOONLIGHT: A cool, soft, low-level blue-silver key, as if moonlight entered from outside the frame. Gentle shadows, dark surroundings, a quiet nocturnal atmosphere, the face softly readable.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 21. Window light

```
LOOK - WINDOW LIGHT: Natural soft side light as if from one large window. It falls off across the face and body, leaving the other side in gentle shadow. Realistic daylight and a calm documentary feel.
GUARD: Use an existing window if there is one; otherwise imply it off-screen. Do not add a visible window or its frame shadows.
```

### 22. Practical

```
LOOK - PRACTICAL: Warm pools of tungsten light across the subject and surfaces, with darker spaces between them, motivated by household lamps.
GUARD: Use existing lamps where present; otherwise keep the implied sources off-screen. Do not add lamps or other props.
```

### 23. Firelight / Candlelight

```
LOOK - FIRELIGHT / CANDLELIGHT: A warm, low orange light from below and to one side, flickering gently (video) like firelight or candlelight across the face and nearby surfaces. Deep dark surroundings and an intimate warm mood.
GUARD: Do not add visible flames or candles unless the scene already has them.
```

### 24. Venetian blinds

```
LOOK - VENETIAN BLINDS: Film-noir contrast and a smoky, dark mood, with directional hard light from one side.
GUARD: Parallel slats of light and shadow ONLY if the scene has blinds, shutters or slatted openings. Otherwise do NOT draw stripes: reproduce the film-noir contrast, hardness and smoky mood with plain hard directional light and dark shadow. Preserve the original geometry.
```

### 25. Dappled / Gobo

```
LOOK - DAPPLED / GOBO: Broken, patterned sunlight feel: soft irregular patches of light and shade across the subject and surfaces, natural movement (video).
GUARD: Leaf-shaped patterns ONLY if there is foliage or an outdoor setting with trees. Otherwise use soft irregular broken light with no recognizable shapes. Do not add foliage.
```

### 26. Volumetric (God rays)

```
LOOK - VOLUMETRIC (GOD RAYS): A light atmospheric haze with bright visible beams cutting diagonally through the scene, as if from an off-screen opening. Rays glow in the haze and catch shoulders and headwear. Dreamy depth.
GUARD: Do not add a window or props. In small or closed spaces, keep the rays subtle and soft.
```

### 27. Overcast / Flat

```
LOOK - OVERCAST / FLAT: Diffused grey-sky daylight with little direction or drama. Very even, soft, almost shadowless light. Muted desaturated colors and a cool neutral grade for a quiet realistic look.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 28. Neon

```
LOOK - NEON: Saturated neon color: warm orange-red light from one side and electric blue light from the other. Colored reflections across the subject and nearby surfaces, rich dark shadows, a night-club mood.
GUARD: Keep the light sources off-screen. Do not add neon signs or tubes.
```

### 29. Teal & Orange

```
LOOK - TEAL & ORANGE: A warm orange key on the subject's skin against cool teal shadows and a teal-toned background. Strong complementary contrast, a cinematic teal-and-orange grade, the original setting and natural skin texture retained.
GUARD: adapt to the scene; do not add objects or light sources that are not plausible here.
```

### 30. Monochrome [BORRADOR]

```
LOOK - MONOCHROME: A black-and-white or near single-hue treatment: strong tonal separation, rich blacks, clean highlights and a controlled contrast curve, so the image reads through light and shape more than color.
GUARD: Choose either true black-and-white or a single tinted hue; do not mix. Skin texture stays natural.
```

### 31. Screen glow

```
LOOK - SCREEN GLOW: A cool blue-white glow on face and hands, as if from a screen just outside the frame, flickering softly (video). The rest of the scene stays dark.
GUARD: Do not add a screen or device.
```

---

## Notas para implementar

- Mantén los dos sets como "formato" seleccionable (Largo / Corto) o define uno por defecto; los IDs de cada set son independientes.
- Looks `[BORRADOR]` (Set 1: 01, 31. Set 2: 11, 14, 30): redactados por nosotros porque no había captura original. Revísalos y quita la etiqueta cuando los valides.
- Intensidad por defecto: Media. Selector Sutil / Media / Fuerte junto al chip de Look.
- Si el usuario sube una referencia (`{REF}`), úsala solo para mood y paleta, nunca para composición. Si no hay referencia, elimina esa línea completa.
- Descripción de tarjetas del modal: en Set 1 usa la línea MOOD de cada look; en Set 2 usa la primera frase de la descripción.
