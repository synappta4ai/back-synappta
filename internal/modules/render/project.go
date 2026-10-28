package render

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Hard limits keep a hostile or buggy project from monopolising the server.
const (
	maxVideoClips = 60
	maxAudioClips = 20
	maxTexts      = 50
	maxTextLen    = 300
	maxDimension  = 1920
)

// Source points at a media asset. Exactly one field is set:
//   - FileID: a library file (files table).
//   - Output: a generated output served under /outputs/ (path or full URL).
type Source struct {
	FileID string `json:"file_id,omitempty"`
	Output string `json:"output,omitempty"`
}

// VideoClip is one entry of the sequential video track. Clips play back to
// back in array order; their timeline position is derived, never stored.
// VideoClip is a clip of the video timeline. Track 0 (or the lowest track in
// use) is the base track and plays sequentially; clips on higher tracks are
// composited on top, positioned at Start.
type VideoClip struct {
	ID     string   `json:"id,omitempty"`
	Kind   string   `json:"kind"` // video | image
	Track  int      `json:"track,omitempty"`
	Source Source   `json:"source"`
	In     float64  `json:"in"`               // seconds into the source (video)
	Out    float64  `json:"out"`              // 0 = until the end of the source (video)
	Length float64  `json:"length,omitempty"` // seconds on screen (image)
	Start  float64  `json:"start,omitempty"`  // absolute position (overlay tracks)
	Volume *float64 `json:"volume,omitempty"` // original audio, 0..2 (default 1)
	// AudioDetached means the front moved this clip's sound to a separate audio
	// clip (linked), so the renderer must not play it twice.
	AudioDetached bool `json:"audio_detached,omitempty"`
}

// AudioClip is a positioned audio layer (music, voice-over). Track is only
// cosmetic for the front: every audio track is mixed.
type AudioClip struct {
	ID     string   `json:"id,omitempty"`
	Track  int      `json:"track,omitempty"`
	Source Source   `json:"source"`
	Start  float64  `json:"start"` // timeline position, seconds
	In     float64  `json:"in"`
	Out    float64  `json:"out"` // 0 = until the end of the source
	Volume *float64 `json:"volume,omitempty"`
}

// TextClip is a caption burned over the video between Start and End.
type TextClip struct {
	ID    string  `json:"id,omitempty"`
	Track int     `json:"track,omitempty"`
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	X     float64 `json:"x"` // 0..1 horizontal anchor (0.5 = centered)
	Y     float64 `json:"y"` // 0..1 vertical anchor
	Size  int     `json:"size"`
	Color string  `json:"color"` // #RRGGBB
}

// Project is the JSON document the front edits and the back renders.
type Project struct {
	Version int         `json:"version"`
	Name    string      `json:"name,omitempty"`
	Width   int         `json:"width"`
	Height  int         `json:"height"`
	FPS     int         `json:"fps"`
	Video   []VideoClip `json:"video"`
	Audio   []AudioClip `json:"audio"`
	Texts   []TextClip  `json:"texts"`
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func finiteNonNeg(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return false
		}
	}
	return true
}

func (s Source) validate() error {
	hasFile, hasOut := strings.TrimSpace(s.FileID) != "", strings.TrimSpace(s.Output) != ""
	if hasFile == hasOut {
		return errors.New("cada clip necesita exactamente una fuente (file_id u output)")
	}
	return nil
}

// normalize fills defaults and validates. It never touches the filesystem.
func (p *Project) normalize() error {
	if p.Width == 0 {
		p.Width = 1280
	}
	if p.Height == 0 {
		p.Height = 720
	}
	if p.FPS == 0 {
		p.FPS = 30
	}
	if p.Width < 64 || p.Height < 64 || p.Width > maxDimension || p.Height > maxDimension {
		return fmt.Errorf("resolucion fuera de rango (64..%d)", maxDimension)
	}
	p.Width -= p.Width % 2 // yuv420p needs even dimensions
	p.Height -= p.Height % 2
	if p.FPS < 12 || p.FPS > 60 {
		return errors.New("fps fuera de rango (12..60)")
	}
	if len(p.Video) == 0 {
		return errors.New("el proyecto no tiene clips de video")
	}
	if len(p.Video) > maxVideoClips || len(p.Audio) > maxAudioClips || len(p.Texts) > maxTexts {
		return errors.New("el proyecto supera el limite de clips")
	}
	for i := range p.Video {
		c := &p.Video[i]
		if c.Kind == "" {
			c.Kind = "video"
		}
		if c.Kind != "video" && c.Kind != "image" {
			return fmt.Errorf("clip de video %d: tipo invalido", i+1)
		}
		if err := c.Source.validate(); err != nil {
			return fmt.Errorf("clip de video %d: %w", i+1, err)
		}
		if c.Track < 0 || !finiteNonNeg(c.In, c.Out, c.Length, c.Start) {
			return fmt.Errorf("clip de video %d: tiempos invalidos", i+1)
		}
		if c.Kind == "image" && c.Length == 0 {
			c.Length = 3
		}
		if c.Volume != nil && (*c.Volume < 0 || *c.Volume > 2 || math.IsNaN(*c.Volume)) {
			return fmt.Errorf("clip de video %d: volumen invalido (0..2)", i+1)
		}
	}
	for i := range p.Audio {
		a := &p.Audio[i]
		if err := a.Source.validate(); err != nil {
			return fmt.Errorf("clip de audio %d: %w", i+1, err)
		}
		if a.Track < 0 || !finiteNonNeg(a.Start, a.In, a.Out) {
			return fmt.Errorf("clip de audio %d: tiempos invalidos", i+1)
		}
		if a.Volume != nil && (*a.Volume < 0 || *a.Volume > 2 || math.IsNaN(*a.Volume)) {
			return fmt.Errorf("clip de audio %d: volumen invalido (0..2)", i+1)
		}
	}
	for i := range p.Texts {
		t := &p.Texts[i]
		t.Text = strings.TrimSpace(t.Text)
		if t.Text == "" || len([]rune(t.Text)) > maxTextLen {
			return fmt.Errorf("texto %d: vacio o demasiado largo", i+1)
		}
		if !finiteNonNeg(t.Start, t.End, t.X, t.Y) || t.X > 1 || t.Y > 1 {
			return fmt.Errorf("texto %d: posicion o tiempos invalidos", i+1)
		}
		if t.Size == 0 {
			t.Size = 48
		}
		if t.Size < 8 || t.Size > 300 {
			return fmt.Errorf("texto %d: tamano fuera de rango", i+1)
		}
		if t.Color == "" {
			t.Color = "#FFFFFF"
		}
		if !hexColor.MatchString(t.Color) {
			return fmt.Errorf("texto %d: color invalido", i+1)
		}
	}
	return nil
}
