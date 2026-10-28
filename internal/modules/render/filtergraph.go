package render

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// resolved is an input already located on disk and measured.
type resolved struct {
	Path string
	Info mediaInfo
}

// plan is everything needed to run ffmpeg: the argument list, the side files
// (drawtext text, written next to the output so paths stay relative) and the
// timeline length used for progress.
type plan struct {
	Args       []string
	TotalSec   float64
	TextFiles  map[string]string // relative file name -> content
	OutputName string
}

func ff(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func vol(v *float64) float64 {
	if v == nil {
		return 1
	}
	return *v
}

const audioNorm = "aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo"

// trimRange clamps In/Out to the measured source duration.
func trimRange(in, out, srcDur float64) (float64, float64, error) {
	if srcDur <= 0 {
		return 0, 0, fmt.Errorf("no se pudo medir la duracion de la fuente")
	}
	if out <= 0 || out > srcDur {
		out = srcDur
	}
	if in < 0 {
		in = 0
	}
	if out-in < 0.05 {
		return 0, 0, fmt.Errorf("el recorte queda vacio")
	}
	return in, out, nil
}

// buildPlan translates a validated Project into an ffmpeg invocation.
// vin/ain are the resolved inputs aligned with p.Video / p.Audio. It is a pure
// function (no I/O) so it can be unit-tested without ffmpeg installed.
// fontFile is the font's file name relative to the working directory ("" =
// let drawtext fall back to fontconfig).
//
// Multi-track: the video track with the lowest index is the base and plays
// sequentially (concatenation). Clips on higher video tracks are composited on
// top at their Start. Every audio track is mixed; text tracks are all burned
// over the composited frame.
func buildPlan(p *Project, vin, ain []resolved, fontFile string, threads int, formats ...string) (*plan, error) {
	format := "mp4"
	if len(formats) > 0 && formats[0] != "" {
		format = formats[0]
	}
	if f, err := normalizeFormat(format); err == nil {
		format = f
	}
	if len(vin) != len(p.Video) || len(ain) != len(p.Audio) {
		return nil, fmt.Errorf("entradas desalineadas con el proyecto")
	}
	fmtSpec := formatOf(format)
	outName := "out." + fmtSpec.Ext
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-nostats", "-progress", "pipe:1"}
	var filters []string
	var concatIn strings.Builder
	files := map[string]string{}
	total := 0.0
	mixN := 0
	var mixIn strings.Builder
	// mixStart appends one delayed audio input to the final amix and returns
	// the label that made it into mixIn.
	mixStart := func(input int, s, e, start, volume float64) {
		ms := int(math.Round(start * 1000))
		label := fmt.Sprintf("m%d", mixN)
		filters = append(filters, fmt.Sprintf("[%d:a]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,%s,volume=%s,adelay=%d|%d[%s]",
			input, ff(s), ff(e), audioNorm, ff(volume), ms, ms, label))
		fmt.Fprintf(&mixIn, "[%s]", label)
		mixN++
	}

	scale := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black,setsar=1,format=yuv420p",
		p.Width, p.Height, p.Width, p.Height)

	// The lowest video track is the base; the rest are overlays on top of it.
	baseTrack := math.MaxInt
	for _, c := range p.Video {
		if c.Track < baseTrack {
			baseTrack = c.Track
		}
	}
	baseClips := 0
	var overlays []struct {
		input int
		start float64
		dur   float64
	}

	for i, c := range p.Video {
		in := vin[i]
		if c.Kind == "image" && !in.Info.HasVideo {
			return nil, fmt.Errorf("clip %d: la imagen no se pudo leer", i+1)
		}
		if c.Kind != "image" && !in.Info.HasVideo {
			return nil, fmt.Errorf("clip %d: la fuente no tiene video", i+1)
		}
		var dur float64
		var s, e float64
		if c.Kind == "image" {
			dur = c.Length
			args = append(args, "-loop", "1", "-framerate", strconv.Itoa(p.FPS), "-t", ff(dur), "-i", in.Path)
			filters = append(filters, fmt.Sprintf("[%d:v]trim=duration=%s,setpts=PTS-STARTPTS,%s[v%d]", i, ff(dur), scale, i))
		} else {
			var err error
			s, e, err = trimRange(c.In, c.Out, in.Info.Duration)
			if err != nil {
				return nil, fmt.Errorf("clip %d: %w", i+1, err)
			}
			dur = e - s
			args = append(args, "-i", in.Path)
			filters = append(filters, fmt.Sprintf("[%d:v]trim=start=%s:end=%s,setpts=PTS-STARTPTS,fps=%d,%s[v%d]",
				i, ff(s), ff(e), p.FPS, scale, i))
		}

		if c.Track == baseTrack {
			// Base track: sound travels with the concatenated stream; silent
			// clips get a matching silent source so concat stays A/V aligned.
			if c.Kind != "image" && in.Info.HasAudio && !c.AudioDetached && vol(c.Volume) > 0 {
				filters = append(filters, fmt.Sprintf("[%d:a]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,%s,volume=%s[a%d]",
					i, ff(s), ff(e), audioNorm, ff(vol(c.Volume)), i))
			} else {
				filters = append(filters, fmt.Sprintf("anullsrc=r=48000:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d]", ff(dur), i))
			}
			fmt.Fprintf(&concatIn, "[v%d][a%d]", i, i)
			baseClips++
			total += dur
			continue
		}
		// Overlay track: placed at its absolute Start, drawn over the base.
		if c.Start+dur > total {
			total = c.Start + dur
		}
		if !fmtSpec.Animated && c.Kind != "image" && in.Info.HasAudio && !c.AudioDetached && vol(c.Volume) > 0 {
			mixStart(i, s, e, c.Start, vol(c.Volume))
		}
		overlays = append(overlays, struct {
			input int
			start float64
			dur   float64
		}{input: i, start: c.Start, dur: dur})
	}

	if baseClips == 0 {
		return nil, fmt.Errorf("el proyecto necesita al menos un clip en la pista base de video")
	}
	filters = append(filters, fmt.Sprintf("%sconcat=n=%d:v=1:a=1[vc][ac]", concatIn.String(), baseClips))

	vLabel := "vc"
	for k, ov := range overlays {
		next := fmt.Sprintf("vx%d", k)
		filters = append(filters, fmt.Sprintf(
			"[%s][v%d]overlay=0:0:eof_action=pass:enable='between(t,%s,%s)'[%s]",
			vLabel, ov.input, ff(ov.start), ff(ov.start+ov.dur), next))
		vLabel = next
	}

	// Text overlays chained over the composited video.
	n := 0
	for _, t := range p.Texts {
		if t.Start >= total {
			continue
		}
		end := t.End
		if end <= t.Start || end > total {
			end = total
		}
		name := fmt.Sprintf("t%d.txt", n)
		files[name] = t.Text
		next := fmt.Sprintf("vt%d", n)
		font := ""
		if fontFile != "" {
			font = ":fontfile=" + fontFile
		}
		filters = append(filters, fmt.Sprintf(
			"[%s]drawtext=textfile=%s:expansion=none%s:fontsize=%d:fontcolor=0x%s:borderw=2:bordercolor=black@0.8:x=(w-text_w)*%s:y=(h-text_h)*%s:enable='between(t,%s,%s)'[%s]",
			vLabel, name, font, t.Size, strings.TrimPrefix(t.Color, "#"),
			ff(t.X), ff(t.Y), ff(t.Start), ff(end), next))
		vLabel = next
		n++
	}

	// Audio layers (every audio track) mixed under the clips' own sound.
	// GIF has no audio track: skip the whole chain so no filter stays unconnected.
	aLabel := "ac"
	inputIdx := len(p.Video) // ffmpeg input index of the next audio source
	for j, a := range p.Audio {
		if fmtSpec.Animated {
			break
		}
		if a.Start >= total {
			continue
		}
		in := ain[j]
		if !in.Info.HasAudio {
			return nil, fmt.Errorf("audio %d: la fuente no tiene sonido", j+1)
		}
		s, e, err := trimRange(a.In, a.Out, in.Info.Duration)
		if err != nil {
			return nil, fmt.Errorf("audio %d: %w", j+1, err)
		}
		args = append(args, "-i", in.Path)
		mixStart(inputIdx, s, e, a.Start, vol(a.Volume))
		inputIdx++
	}
	if mixN > 0 {
		filters = append(filters, fmt.Sprintf("[ac]%samix=inputs=%d:duration=first:dropout_transition=0:normalize=0[aout]", mixIn.String(), mixN+1))
		aLabel = "aout"
	}

	// GIF: paleta de color en una pasada y sin audio.
	finalVideo := vLabel
	if fmtSpec.Animated {
		finalVideo = "vgif"
		filters = append(filters, fmt.Sprintf(
			"[%s]fps=12,scale=480:480:force_original_aspect_ratio=decrease:flags=lanczos,split[gs0][gs1];[gs0]palettegen=stats_mode=diff[gp];[gs1][gp]paletteuse=dither=bayer:bayer_scale=5[%s]",
			vLabel, finalVideo))
	}

	maps := []string{"-map", "[" + finalVideo + "]"}
	if !fmtSpec.Animated {
		maps = append(maps, "-map", "["+aLabel+"]")
	}
	args = append(args, "-filter_complex", strings.Join(filters, ";"))
	args = append(args, maps...)
	args = append(args, "-t", ff(total), "-r", strconv.Itoa(p.FPS))
	args = append(args, fmtSpec.VideoArgs...)
	if !fmtSpec.Animated {
		args = append(args, fmtSpec.AudioArgs...)
	}
	if fmtSpec.Ext == "mp4" || fmtSpec.Ext == "mov" {
		args = append(args, "-movflags", "+faststart")
	}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args, outName)
	return &plan{Args: args, TotalSec: total, TextFiles: files, OutputName: outName}, nil
}
