package storage

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// AWS publishes this presigned-URL example as a test case:
// https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sigv4-query-string-auth.html
// If the canonical request, string to sign or signing-key derivation drift,
// the signature stops matching this constant.
func TestPresignMatchesAWSExample(t *testing.T) {
	t.Parallel()

	// El ejemplo de AWS usa virtual-hosted style; el presign toma el URI
	// canónico tal cual, así que alcanza con apuntar el endpoint al bucket.
	ep, err := url.Parse("https://examplebucket.s3.amazonaws.com")
	if err != nil {
		t.Fatal(err)
	}
	p := &Presigner{
		endpoint:  ep,
		bucket:    "examplebucket",
		region:    "us-east-1",
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		ttl:       24 * time.Hour,
	}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

	got := p.presign("GET", "/test.txt",
		map[string]string{"host": "examplebucket.s3.amazonaws.com"},
		now, 24*time.Hour)

	const want = "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z" +
		"&X-Amz-Expires=86400" +
		"&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presigned URL mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestPresignPutPathStyleAndEncoding(t *testing.T) {
	t.Parallel()

	ep, err := url.Parse("https://bucket.example.com")
	if err != nil {
		t.Fatal(err)
	}
	p := &Presigner{
		endpoint:  ep,
		bucket:    "media",
		region:    "us-east-1",
		accessKey: "AK",
		secretKey: "SK",
		ttl:       15 * time.Minute,
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	key := "synapta/images/2026/10/foto con espacios + señal.png"

	raw := p.PresignPut(key, "image/png", now)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("presigned URL no parsea: %v", err)
	}

	if u.Scheme != "https" || u.Host != "bucket.example.com" {
		t.Errorf("endpoint inesperado: %s://%s", u.Scheme, u.Host)
	}
	if want := "/media/synapta/images/2026/10/foto%20con%20espacios%20%2B%20se%C3%B1al.png"; u.EscapedPath() != want {
		t.Errorf("path = %s, want %s", u.EscapedPath(), want)
	}
	// The key's slashes must stay separators (they are part of the object key,
	// not of the encoding), so the request path nests exactly like the key.
	if !strings.HasPrefix(raw, "https://bucket.example.com/media/synapta/images/2026/10/foto%20") {
		t.Errorf("path sin separadores del key: %s", raw)
	}

	q := u.Query()
	if q.Get("X-Amz-Algorithm") != Algorithm {
		t.Errorf("algorithm = %q", q.Get("X-Amz-Algorithm"))
	}
	if q.Get("X-Amz-Expires") != "900" {
		t.Errorf("expires = %q, want 900", q.Get("X-Amz-Expires"))
	}
	if got := q.Get("X-Amz-SignedHeaders"); got != "content-type;host" {
		t.Errorf("signed headers = %q, want content-type;host", got)
	}
	if q.Get("X-Amz-Credential") != "AK/20261020/us-east-1/s3/aws4_request" {
		t.Errorf("credential = %q", q.Get("X-Amz-Credential"))
	}
	if len(q.Get("X-Amz-Signature")) != 64 {
		t.Errorf("firma inesperada: %q", q.Get("X-Amz-Signature"))
	}
}

func TestPresignPutWithoutContentTypeSignsOnlyHost(t *testing.T) {
	t.Parallel()

	ep, _ := url.Parse("http://127.0.0.1:19000")
	p := &Presigner{endpoint: ep, bucket: "b", region: "us-east-1", accessKey: "AK", secretKey: "SK", ttl: time.Minute}

	u, err := url.Parse(p.PresignPut("k.png", "", time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("X-Amz-SignedHeaders"); got != "host" {
		t.Errorf("signed headers = %q, want host", got)
	}
}

func TestHeadBucketURL(t *testing.T) {
	t.Parallel()

	ep, _ := url.Parse("https://bucket.example.com")
	p := &Presigner{endpoint: ep, bucket: "media", region: "us-east-1", accessKey: "AK", secretKey: "SK", ttl: time.Minute}

	u, err := url.Parse(p.HeadBucketURL(time.Unix(0, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.EscapedPath(); got != "/media" {
		t.Errorf("path = %q, want /media", got)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Error("HEAD sin firma")
	}
}

func TestPresignDeleteTargetsSameKey(t *testing.T) {
	t.Parallel()

	ep, _ := url.Parse("https://bucket.example.com")
	p := &Presigner{endpoint: ep, bucket: "media", region: "us-east-1", accessKey: "AK", secretKey: "SK", ttl: time.Minute}
	now := time.Unix(0, 0)

	u, err := url.Parse(p.PresignDelete("a/b.png", now))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.EscapedPath(); got != "/media/a/b.png" {
		t.Errorf("path = %q", got)
	}
	if u.Query().Get("X-Amz-Signature") == "" {
		t.Error("DELETE sin firma")
	}
	if p.PresignDelete("a/b.png", now) == p.PresignPut("a/b.png", "", now) {
		t.Error("DELETE y PUT comparten firma (el método no entra en el canonical request)")
	}
}

func TestPresignPutHonorsEndpointPathPrefix(t *testing.T) {
	t.Parallel()

	ep, _ := url.Parse("https://proxy.example.com/s3/")
	p := &Presigner{endpoint: ep, bucket: "media", region: "us-east-1", accessKey: "AK", secretKey: "SK", ttl: time.Minute}

	raw := p.PresignPut("a/b.png", "image/png", time.Unix(0, 0))
	if !strings.HasPrefix(raw, "https://proxy.example.com/s3/media/a/b.png?") {
		t.Errorf("prefijo del endpoint perdido: %s", raw)
	}
}

func TestPublicURL(t *testing.T) {
	t.Parallel()

	ep, _ := url.Parse("https://bucket.example.com")

	pathStyle := &Presigner{endpoint: ep, bucket: "media"}
	if got, want := pathStyle.PublicURL("a/foto 1.png"), "https://bucket.example.com/media/a/foto%201.png"; got != want {
		t.Errorf("path-style = %s, want %s", got, want)
	}

	cdn := &Presigner{endpoint: ep, bucket: "media", publicBase: "https://cdn.example.com"}
	if got, want := cdn.PublicURL("a/foto 1.png"), "https://cdn.example.com/a/foto%201.png"; got != want {
		t.Errorf("public base = %s, want %s", got, want)
	}
}

func TestNewRequiresCompleteConfig(t *testing.T) {
	t.Parallel()

	base := Config{
		Endpoint:  "https://bucket.example.com",
		Bucket:    "media",
		AccessKey: "AK",
		SecretKey: "SK",
	}
	if _, err := New(base); err != nil {
		t.Fatalf("config completa rechazada: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"sin endpoint":       func(c *Config) { c.Endpoint = "" },
		"sin bucket":         func(c *Config) { c.Bucket = "  " },
		"sin access":         func(c *Config) { c.AccessKey = "" },
		"sin secret":         func(c *Config) { c.SecretKey = "" },
		"endpoint ftp":       func(c *Config) { c.Endpoint = "ftp://bucket.example.com" },
		"endpoint s/esquema": func(c *Config) { c.Endpoint = "bucket.example.com" },
		"endpoint con query": func(c *Config) { c.Endpoint = "https://h/x?y=1" },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("New() aceptó config inválida: %s", name)
		}
	}
}

func TestNewDefaultsAndClamps(t *testing.T) {
	t.Parallel()

	base := Config{
		Endpoint:  "https://bucket.example.com/",
		Bucket:    " media ",
		AccessKey: "AK",
		SecretKey: "SK",
	}
	p, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if p.TTL() != DefaultTTL {
		t.Errorf("TTL cero = %s, want %s", p.TTL(), DefaultTTL)
	}
	if p.Bucket() != "media" {
		t.Errorf("bucket = %q (se espera trimmed)", p.Bucket())
	}
	if p.endpoint.Path != "" {
		t.Errorf("endpoint conserva slash final: %q", p.endpoint.Path)
	}

	base.TTL = 30 * time.Second
	if p, err = New(base); err != nil {
		t.Fatal(err)
	}
	if p.TTL() != 30*time.Second {
		t.Errorf("TTL = %s, want 30s", p.TTL())
	}

	base.TTL = 30 * 24 * time.Hour
	if p, err = New(base); err != nil {
		t.Fatal(err)
	}
	if p.TTL() != maxPresignTTL {
		t.Errorf("TTL = %s, want clamp a %s", p.TTL(), maxPresignTTL)
	}
}

func TestUriEncode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"simple.png":        "simple.png",
		"a b":               "a%20b",
		"a+b":               "a%2Bb",
		"ñandú":             "%C3%B1and%C3%BA",
		"a&b=c":             "a%26b%3Dc",
		"ruta/with space/x": "ruta/with%20space/x",
	}
	for in, want := range cases {
		if got := uriEncode(in, false); got != want {
			t.Errorf("uriEncode(%q) = %q, want %q", in, got, want)
		}
	}
	// Query values encode the slash (AWS UriEncode encodeSlash=true).
	if got := uriEncode("AK/20261020/us-east-1/s3/aws4_request", true); got != "AK%2F20261020%2Fus-east-1%2Fs3%2Faws4_request" {
		t.Errorf("uriEncode query = %q", got)
	}
}
