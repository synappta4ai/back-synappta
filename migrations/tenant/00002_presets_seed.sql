-- +goose Up
-- +goose StatementBegin

INSERT INTO preset_groups (name, slug, description) VALUES
    ('Lens', 'lens', 'Camera lens presets — focal length, depth of field, and optical character'),
    ('Camera Body', 'camera', 'Camera body presets — sensor, color science, and capture aesthetics'),
    ('Camera Motion', 'cameraMotion', 'Camera motion presets — movement, stabilization, and dynamic feel'),
    ('Color Grading', 'colorGrading', 'Color grading presets — look, palette, and mood'),
    ('Genre', 'genre', 'Genre presets — narrative tone, pacing, and visual conventions');

INSERT INTO presets (group_id, code, label, prompt) VALUES
    ((SELECT id FROM preset_groups WHERE slug = 'lens'), 'wide_24mm', '24mm Wide',
     'shot on a 24mm wide lens, expansive framing with subtle edge distortion, deep depth of field'),
    ((SELECT id FROM preset_groups WHERE slug = 'lens'), 'classic_35mm', '35mm Classic',
     'shot on a 35mm lens, natural human perspective, balanced framing, classic cinema feel'),
    ((SELECT id FROM preset_groups WHERE slug = 'lens'), 'portrait_50mm', '50mm Portrait',
     'shot on a 50mm lens, intimate perspective, clean subject isolation, natural compression'),
    ((SELECT id FROM preset_groups WHERE slug = 'lens'), 'tele_85mm', '85mm Tele',
     'shot on an 85mm lens, creamy shallow depth of field, compressed background, cinematic bokeh'),
    ((SELECT id FROM preset_groups WHERE slug = 'camera'), 'arri_alexa', 'Arri Alexa 65',
     'captured on Arri Alexa 65, rich dynamic range, organic highlight rolloff, filmic skin tones'),
    ((SELECT id FROM preset_groups WHERE slug = 'camera'), 'film_16mm', '16mm Film',
     'shot on 16mm celluloid film, visible grain structure, slight halation, analog imperfection and warmth'),
    ((SELECT id FROM preset_groups WHERE slug = 'cameraMotion'), 'static_lockoff', 'Static / Locked Off',
     'static locked-off camera, no camera movement, tripod-mounted, steady composition'),
    ((SELECT id FROM preset_groups WHERE slug = 'cameraMotion'), 'slow_dolly_in', 'Slow Dolly In',
     'slow dolly push into the subject, smooth and deliberate, building intimacy and focus'),
    ((SELECT id FROM preset_groups WHERE slug = 'cameraMotion'), 'handheld', 'Handheld',
     'handheld camera, organic breathing motion, intimate and immediate, documentary feel'),
    ((SELECT id FROM preset_groups WHERE slug = 'colorGrading'), 'tokio', 'Tokio',
     'neon-drenched cyberpunk palette, magenta and cyan highlights, deep indigo shadows, cinematic urban night'),
    ((SELECT id FROM preset_groups WHERE slug = 'colorGrading'), 'colombia', 'Colombia',
     'warm vibrant tropical palette, amber and emerald tones, golden hour warmth, rich saturated colors'),
    ((SELECT id FROM preset_groups WHERE slug = 'genre'), 'drama', 'Drama',
     'dramatic narrative pacing, emotional character focus, naturalistic performances, restrained camera'),
    ((SELECT id FROM preset_groups WHERE slug = 'genre'), 'action', 'Action',
     'high-energy action sequencing, rapid cuts, dynamic camera movement, intense stunts and choreography');

-- +goose StatementEnd

-- +goose Down
DELETE FROM presets;
DELETE FROM preset_groups;
