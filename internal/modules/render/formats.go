package render

import (
	"fmt"
	"strings"
)

// exportFormat describes how the final file is encoded.
type exportFormat struct {
	Key       string // canonical value stored with the job
	Ext       string // out.<Ext>
	MimeType  string
	Animated  bool // gif: no audio track
	VideoArgs []string
	AudioArgs []string
}

// formats is the allow-list the API accepts.
var formats = map[string]exportFormat{
	"mp4": {
		Key: "mp4", Ext: "mp4", MimeType: "video/mp4",
		VideoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p"},
		AudioArgs: []string{"-c:a", "aac", "-b:a", "160k", "-ar", "48000"},
	},
	"mp4_h265": {
		Key: "mp4_h265", Ext: "mp4", MimeType: "video/mp4",
		VideoArgs: []string{"-c:v", "libx265", "-preset", "medium", "-crf", "28", "-pix_fmt", "yuv420p", "-tag:v", "hvc1"},
		AudioArgs: []string{"-c:a", "aac", "-b:a", "160k", "-ar", "48000"},
	},
	"mov": {
		Key: "mov", Ext: "mov", MimeType: "video/quicktime",
		VideoArgs: []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p"},
		AudioArgs: []string{"-c:a", "aac", "-b:a", "192k", "-ar", "48000"},
	},
	"webm": {
		Key: "webm", Ext: "webm", MimeType: "video/webm",
		VideoArgs: []string{"-c:v", "libvpx-vp9", "-b:v", "0", "-crf", "34", "-row-mt", "1", "-pix_fmt", "yuv420p"},
		AudioArgs: []string{"-c:a", "libopus", "-b:a", "96k"},
	},
	"gif": {
		Key: "gif", Ext: "gif", MimeType: "image/gif", Animated: true,
		VideoArgs: []string{"-loop", "0"},
	},
}

// normalizeFormat maps aliases to a canonical key (empty = mp4).
func normalizeFormat(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "mp4", "h264", "avc":
		return "mp4", nil
	case "mp4_h265", "h265", "hevc":
		return "mp4_h265", nil
	case "mov", "quicktime":
		return "mov", nil
	case "webm", "vp9":
		return "webm", nil
	case "gif":
		return "gif", nil
	}
	return "", fmt.Errorf("formato no soportado: %s", raw)
}

func formatOf(key string) exportFormat {
	if f, ok := formats[key]; ok {
		return f
	}
	return formats["mp4"]
}
