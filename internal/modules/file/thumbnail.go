package file

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/disintegration/imaging"
)

// makeThumbnail renders a 300x300 thumbnail from the uploaded bytes and
// returns it with the content type to store. Doing it in memory keeps the
// bucket path free of disk round-trips.
func makeThumbnail(data []byte, ext string) ([]byte, string, error) {
	src, err := imaging.Decode(bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		return nil, "", fmt.Errorf("failed to decode image: %w", err)
	}
	dst := imaging.Fit(src, 300, 300, imaging.Lanczos)

	var buf bytes.Buffer
	contentType := "image/jpeg"
	format := imaging.JPEG
	opts := []imaging.EncodeOption{imaging.JPEGQuality(80)}

	switch strings.ToLower(ext) {
	case ".png":
		contentType, format = "image/png", imaging.PNG
		opts = []imaging.EncodeOption{imaging.PNGCompressionLevel(6)}
	case ".gif":
		// imaging encodes GIFs as PNG-friendly images; keep PNG bytes.
		contentType, format, opts = "image/png", imaging.PNG, nil
	}

	if err := imaging.Encode(&buf, dst, format, opts...); err != nil {
		return nil, "", fmt.Errorf("failed to encode thumbnail: %w", err)
	}
	return buf.Bytes(), contentType, nil
}
