package infoproduct

import (
	"encoding/json"
	"math"
	"strings"
)

const maxSegments = 6

// ComposeTarget are the limits of the video model the user picked. The front
// already knows them from the model catalog and sends them along.
type ComposeTarget struct {
	Model          string   `json:"model"`
	MaxDuration    int      `json:"max_duration"`
	Durations      []int    `json:"durations"`
	Ratios         []string `json:"ratios"`
	Resolutions    []string `json:"resolutions"`
	SupportsAudio  bool     `json:"supports_audio"`
	AudioReference bool     `json:"audio_reference"`
	MaxReferences  int      `json:"max_references"`
}

// Ingredient is one product image attached to the generation.
type Ingredient struct {
	FileID string `json:"file_id"`
	Tag    string `json:"tag"`
	Role   string `json:"role"`
}

// AudioRef is one audio file attached as music or voice-over reference.
type AudioRef struct {
	FileID string `json:"file_id"`
	Tag    string `json:"tag"`
	Kind   string `json:"kind"`
	Note   string `json:"note"`
}

// VideoRef is one reference clip attached as a visual reference (camera
// work, framing and edit rhythm). It is declared in the prompt and sent to
// the generator as a video content item.
type VideoRef struct {
	FileID string `json:"file_id"`
	Tag    string `json:"tag"`
	Name   string `json:"name"`
}

// Segment is one generation of a multi-clip replica.
type Segment struct {
	Index  int     `json:"index"`
	Start  float64 `json:"start"`
	End    float64 `json:"end"`
	Prompt string  `json:"prompt"`
}

// VideoPrompt is the result of compose: everything the Studio-style panel
// needs to launch the generation(s).
type VideoPrompt struct {
	Prompt        string       `json:"prompt"`
	Model         string       `json:"model"`
	Duration      int          `json:"duration"`
	Ratio         string       `json:"ratio"`
	Resolution    string       `json:"resolution"`
	GenerateAudio bool         `json:"generate_audio"`
	Ingredients   []Ingredient `json:"ingredients"`
	AudioRefs     []AudioRef   `json:"audio_refs"`
	VideoRefs     []VideoRef   `json:"video_refs"`
	Segments      []Segment    `json:"segments"`
	Warnings      []string     `json:"warnings"`
}

// segmentsRequired is how many generations the replica needs: the reference
// length divided by what the model can produce in one clip.
func segmentsRequired(total float64, maxDuration int) int {
	if total <= 0 || maxDuration <= 0 {
		return 1
	}
	n := int(math.Ceil(total/float64(maxDuration) - 1e-9))
	if n < 1 {
		return 1
	}
	if n > maxSegments {
		return maxSegments
	}
	return n
}

// pickDuration returns the allowed duration closest to the wanted one without
// exceeding maxDuration (when set). With no list, it just clamps.
func pickDuration(want int, allowed []int, maxDuration int) int {
	if maxDuration > 0 && want > maxDuration {
		want = maxDuration
	}
	if want < 1 {
		want = 1
	}
	if len(allowed) == 0 {
		return want
	}
	best, found := 0, false
	for _, d := range allowed {
		if maxDuration > 0 && d > maxDuration {
			continue
		}
		if d <= want && (!found || d > best) {
			best, found = d, true
		}
	}
	if found {
		return best
	}
	// Nothing at or below the wanted value: take the smallest allowed.
	smallest := allowed[0]
	for _, d := range allowed {
		if d < smallest {
			smallest = d
		}
	}
	return smallest
}

// pickString returns want when allowed, else preferred when allowed, else the
// first allowed entry. An empty allowed list accepts want as is.
func pickString(want, preferred string, allowed []string) string {
	if len(allowed) == 0 {
		return want
	}
	for _, a := range allowed {
		if strings.EqualFold(a, want) {
			return a
		}
	}
	for _, a := range allowed {
		if strings.EqualFold(a, preferred) {
			return a
		}
	}
	return allowed[0]
}

// composeRaw is what the brain returns for compose.
type composeRaw struct {
	Prompt        string   `json:"prompt"`
	Duration      int      `json:"duration"`
	Ratio         string   `json:"ratio"`
	Resolution    string   `json:"resolution"`
	GenerateAudio bool     `json:"generate_audio"`
	ImageRoles    []string `json:"image_roles"`
	AudioRoles    []struct {
		Kind string `json:"kind"`
		Note string `json:"note"`
	} `json:"audio_roles"`
	Segments []Segment `json:"segments"`
	Warnings []string  `json:"warnings"`
}

// buildVideoPrompt validates the brain's answer against the model limits and
// attaches the real file ids to the @ImageN / @AudioN tags.
func buildVideoPrompt(raw json.RawMessage, target ComposeTarget, imageIDs, audioIDs []string, total float64, refRatio string, segmentsWanted int) (*VideoPrompt, error) {
	var r composeRaw
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &VideoPrompt{
		Prompt:      strings.TrimSpace(r.Prompt),
		Model:       target.Model,
		Warnings:    append([]string{}, r.Warnings...),
		Ingredients: []Ingredient{},
		AudioRefs:   []AudioRef{},
	}
	out.Duration = pickDuration(r.Duration, target.Durations, target.MaxDuration)
	out.Ratio = pickString("9:16", pickString(r.Ratio, refRatio, target.Ratios), target.Ratios)
	// The output is watched on smartphones: vertical whenever the model allows
	// it (otherwise the brain's or the reference's closest allowed ratio).
	out.Resolution = pickString(r.Resolution, "", target.Resolutions)
	out.GenerateAudio = r.GenerateAudio && target.SupportsAudio

	for i, id := range imageIDs {
		ing := Ingredient{FileID: id, Tag: "@Image" + itoa(i+1)}
		if i < len(r.ImageRoles) {
			ing.Role = r.ImageRoles[i]
		}
		out.Ingredients = append(out.Ingredients, ing)
	}
	for i, id := range audioIDs {
		ref := AudioRef{FileID: id, Tag: "@Audio" + itoa(i+1), Kind: "music"}
		if i < len(r.AudioRoles) {
			if k := strings.ToLower(strings.TrimSpace(r.AudioRoles[i].Kind)); k == "voiceover" || k == "music" {
				ref.Kind = k
			}
			ref.Note = r.AudioRoles[i].Note
		}
		out.AudioRefs = append(out.AudioRefs, ref)
	}

	out.Segments = r.Segments
	if len(out.Segments) == 0 {
		out.Segments = []Segment{{Index: 1, Start: 0, End: float64(out.Duration), Prompt: out.Prompt}}
	}
	if out.Prompt == "" && len(out.Segments) > 0 {
		out.Prompt = out.Segments[0].Prompt
	}
	for i := range out.Segments {
		out.Segments[i].Index = i + 1
	}
	if segmentsWanted > 1 && len(out.Segments) != segmentsWanted {
		out.Warnings = append(out.Warnings,
			"El video de referencia necesita "+itoa(segmentsWanted)+" generaciones y el cerebro propuso "+itoa(len(out.Segments))+": revisa los segmentos.")
	}
	if total > 0 && segmentsWanted <= 1 && target.MaxDuration > 0 && int(math.Ceil(total)) > target.MaxDuration {
		out.Warnings = append(out.Warnings, "La duracion se ajusto al maximo del modelo.")
	}
	if out.Prompt == "" {
		return nil, errEmptyPrompt
	}
	return out, nil
}
