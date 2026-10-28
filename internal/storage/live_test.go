package storage

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestPresignPutLive signs a real PUT and checks the store accepts it. It is
// skipped unless a bucket is provided:
//
//	MINIO_TEST_ENDPOINT=http://127.0.0.1:19000 \
//	MINIO_TEST_BUCKET=synapta-test \
//	MINIO_TEST_ACCESS_KEY=... MINIO_TEST_SECRET_KEY=... \
//	go test ./internal/storage -run TestPresignPutLive -v
//
// With MINIO_TEST_PUBLIC_READ=1 the object is also downloaded through
// PublicURL (the bucket must allow anonymous GET).
// deleteObject removes an object through its presigned DELETE URL.
func deleteObject(t *testing.T, url, key string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Errorf("no se pudo armar el DELETE de %s: %v", key, err)
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Errorf("DELETE %s falló: %v", key, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent && res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(res.Body)
		t.Errorf("DELETE %s: status %d (%s)", key, res.StatusCode, msg)
	}
}

func TestPresignPutLive(t *testing.T) {
	endpoint := os.Getenv("MINIO_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_TEST_ENDPOINT no seteado: test live omitido")
	}
	bucket := os.Getenv("MINIO_TEST_BUCKET")
	if bucket == "" {
		bucket = "synapta-test"
	}
	ep, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("endpoint inválido: %v", err)
	}

	p := &Presigner{
		endpoint:  ep,
		bucket:    bucket,
		region:    "us-east-1",
		accessKey: os.Getenv("MINIO_TEST_ACCESS_KEY"),
		secretKey: os.Getenv("MINIO_TEST_SECRET_KEY"),
		ttl:       5 * time.Minute,
	}
	if p.accessKey == "" || p.secretKey == "" {
		t.Skip("MINIO_TEST_ACCESS_KEY / MINIO_TEST_SECRET_KEY no seteados")
	}

	body := make([]byte, 1024)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("e2e/%d.bin", time.Now().UnixNano())
	uploadURL := p.PresignPut(key, "application/octet-stream", time.Now())

	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT al bucket falló: %v", err)
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: status %d (%s)", key, res.StatusCode, msg)
	}
	t.Logf("PUT OK %s (%d bytes)", key, len(body))

	// Self-cleaning: the test leaves the bucket exactly as it was.
	t.Cleanup(func() { deleteObject(t, p.PresignDelete(key, time.Now()), key) })

	if os.Getenv("MINIO_TEST_PUBLIC_READ") != "1" {
		return
	}
	got, err := http.Get(p.PublicURL(key))
	if err != nil {
		t.Fatalf("GET público falló: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", p.PublicURL(key), got.StatusCode)
	}
	data, _ := io.ReadAll(got.Body)
	if !bytes.Equal(data, body) {
		t.Fatalf("contenido distinto: %d bytes vs %d", len(data), len(body))
	}
	t.Logf("GET público OK %s", p.PublicURL(key))
}
