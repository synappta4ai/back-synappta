package file

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestStorageConfigLiveUpload configured a tenant bucket in the database, lets
// the service build the presigner from that row (decrypting the credentials)
// and uploads through the signed URL to a real MinIO/S3.
//
//	TEST_DATABASE_URL=postgres://... \
//	MINIO_TEST_ENDPOINT=https://bucket.example.com MINIO_TEST_BUCKET=dev \
//	MINIO_TEST_ACCESS_KEY=... MINIO_TEST_SECRET_KEY=... \
//	go test ./internal/modules/file -run TestStorageConfigLiveUpload -v
func TestStorageConfigLiveUpload(t *testing.T) {
	if os.Getenv("MINIO_TEST_ENDPOINT") == "" {
		t.Skip("MINIO_TEST_ENDPOINT no seteado")
	}
	store, _ := newTestStore(t) // skips without TEST_DATABASE_URL
	accessKey, secretKey := os.Getenv("MINIO_TEST_ACCESS_KEY"), os.Getenv("MINIO_TEST_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("MINIO_TEST_ACCESS_KEY / MINIO_TEST_SECRET_KEY no seteados")
	}
	bucket := os.Getenv("MINIO_TEST_BUCKET")
	if bucket == "" {
		bucket = "synapta-test"
	}
	svc := NewService(store, "", "acme", testEncKey, 10<<20, false)

	// SaveStorageConfig corre el probe real (HEAD firmado) contra el bucket.
	cfg, err := svc.SaveStorageConfig(StorageConfigRequest{
		Endpoint:  os.Getenv("MINIO_TEST_ENDPOINT"),
		Bucket:    bucket,
		Region:    "us-east-1",
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("SaveStorageConfig (probe real): %v", err)
	}
	t.Cleanup(func() { _ = svc.DeleteStorageConfig() })
	if !cfg.Configured || cfg.Bucket != bucket {
		t.Fatalf("config inesperada: %+v", cfg)
	}

	body := make([]byte, 1024)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	res, err := svc.PresignUpload(PresignRequest{Filename: "tenant.png", ContentType: "image/png", Size: int64(len(body))})
	if err != nil {
		t.Fatalf("PresignUpload: %v", err)
	}
	if res.Bucket != bucket {
		t.Errorf("bucket del presign = %q, want %q", res.Bucket, bucket)
	}

	req, err := http.NewRequest(http.MethodPut, res.UploadURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range res.Headers {
		req.Header.Set(name, value)
	}
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer put.Body.Close()
	msg, _ := io.ReadAll(put.Body)
	if put.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: %d (%s)", res.Key, put.StatusCode, msg)
	}
	t.Logf("upload OK con config del tenant: %s → %s", res.Key, res.Bucket)

	// Limpieza con el presigner de la config guardada.
	presigner, err := svc.presigner()
	if err != nil || presigner == nil {
		t.Fatalf("presigner: %v", err)
	}
	del, err := http.NewRequest(http.MethodDelete, presigner.PresignDelete(res.Key, time.Now()), nil)
	if err != nil {
		t.Fatal(err)
	}
	dres, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer dres.Body.Close()
	if dres.StatusCode >= 300 {
		t.Fatalf("DELETE %s: %d", res.Key, dres.StatusCode)
	}
}
