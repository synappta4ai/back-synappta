package file

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/disintegration/imaging"
)

// La tabla propia cubre los tipos que genera la plataforma: sin ella,
// mime.TypeByExtension devuelve "" para .mp4 en un sistema sin base MIME y el
// archivo quedaba como application/octet-stream (el front lo pintaba como
// documento genérico).
func TestMimeForExtCoversLibraryTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		".mp4":  "video/mp4",
		".m4v":  "video/x-m4v",
		".mov":  "video/quicktime",
		".webm": "video/webm",
		".webp": "image/webp",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".mp3":  "audio/mpeg",
		".wav":  "audio/wav",
		".ogg":  "audio/ogg",
		".m4a":  "audio/mp4",
	}
	for ext, want := range cases {
		if got := mimeForExt(ext); got != want {
			t.Errorf("mimeForExt(%q) = %q, want %q", ext, got, want)
		}
	}
	if got := mimeForExt(".xyz"); got != "" {
		t.Errorf("una extensión desconocida no debería inventar MIME: %q", got)
	}
}

// Las filas viejas guardadas como octet-stream se corrigen al responder, sin
// tocar la base.
func TestNormalizedMimeFixesLegacyRows(t *testing.T) {
	t.Parallel()

	legacy := &File{Format: "mp4", MimeType: "application/octet-stream"}
	if got := normalizedMime(legacy); got != "video/mp4" {
		t.Errorf("legacy mp4 = %q, want video/mp4", got)
	}
	if legacy.MimeType != "application/octet-stream" {
		t.Error("la fila no debería mutarse en memoria")
	}
	known := &File{Format: "png", MimeType: "image/png"}
	if got := normalizedMime(known); got != "image/png" {
		t.Errorf("un mime válido no se toca: %q", got)
	}
	unknown := &File{Format: "xyz", MimeType: "application/octet-stream"}
	if got := normalizedMime(unknown); got != "application/octet-stream" {
		t.Errorf("sin formato conocido se deja igual: %q", got)
	}
}

// webp8x8 es un WebP real de 8x8 generado con Chromium (canvas.toDataURL).
// WebP necesita el decodificador de golang.org/x/image registrado en
// image.Decode: imaging por sí solo no lo decodifica.
const webp8x8 = "UklGRlQCAABXRUJQVlA4WAoAAAAgAAAABwAABwAASUNDUMgBAAAAAAHIAAAAAAQwAABtbnRyUkdCIFhZWiAH4AABAAEAAAAAAABhY3NwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAA9tYAAQAAAADTLQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAlkZXNjAAAA8AAAACRyWFlaAAABFAAAABRnWFlaAAABKAAAABRiWFlaAAABPAAAABR3dHB0AAABUAAAABRyVFJDAAABZAAAAChnVFJDAAABZAAAAChiVFJDAAABZAAAAChjcHJ0AAABjAAAADxtbHVjAAAAAAAAAAEAAAAMZW5VUwAAAAgAAAAcAHMAUgBHAEJYWVogAAAAAAAAb6IAADj1AAADkFhZWiAAAAAAAABimQAAt4UAABjaWFlaIAAAAAAAACSgAAAPhAAAts9YWVogAAAAAAAA9tYAAQAAAADTLXBhcmEAAAAAAAQAAAACZmYAAPKnAAANWQAAE9AAAApbAAAAAAAAAABtbHVjAAAAAAAAAAEAAAAMZW5VUwAAACAAAAAcAEcAbwBvAGcAbABlACAASQBuAGMALgAgADIAMAAxADZWUDggZgAAALACAJ0BKggACAABQCYloAAQ5j+m8gB6oBuTddycQAD+9Bfub0Dcf3/zc8LvX/Id7XYxeNb/5U+74dB340nP4t8c8WwZFsHkSiUBan26jAf//U5j/8dn8mS//gh//7dn//t2f9vwAA=="

func TestMakeThumbnailDecodesWebP(t *testing.T) {
	t.Parallel()

	data, err := base64.StdEncoding.DecodeString(webp8x8)
	if err != nil {
		t.Fatalf("fixture webp: %v", err)
	}
	thumb, contentType, err := makeThumbnail(data, ".webp")
	if err != nil {
		t.Fatalf("makeThumbnail(webp): %v", err)
	}
	if contentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", contentType)
	}
	if _, err := imaging.Decode(bytes.NewReader(thumb)); err != nil {
		t.Fatalf("la miniatura generada no es una imagen válida: %v", err)
	}
}
