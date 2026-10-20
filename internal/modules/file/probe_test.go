package file

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"synapta/internal/storage"
)

// consoleLikeEndpoint imita la consola web de MinIO: su SPA contesta 200 con
// HTML a cualquier método y path. Es el caso real que motivó la verificación
// por escritura: un HEAD al bucket "pasaba" y después los archivos se perdían.
func consoleLikeEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<!doctype html><html><head><base href=\"/\"/></head></html>"))
	}))
	t.Cleanup(server.Close)
	return server
}

func presignerFor(t *testing.T, endpoint, bucket string) *storage.Presigner {
	t.Helper()
	p, err := storage.New(storage.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		Region:    "us-east-1",
		AccessKey: "ak",
		SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	return p
}

func TestProbeBucketRejectsNonS3Endpoint(t *testing.T) {
	server := consoleLikeEndpoint(t)
	err := probeBucket(presignerFor(t, server.URL, "media"))
	if err == nil {
		t.Fatal("un endpoint que responde 200 a todo debería fallar la verificación")
	}
	if !strings.Contains(err.Error(), "API S3") {
		t.Fatalf("error poco claro para el admin: %v", err)
	}
}

func TestProbeBucketRoundTripLeavesNothingBehind(t *testing.T) {
	fake := newFakeS3(t, "media")
	if err := probeBucket(presignerFor(t, fake.endpoint, fake.bucket)); err != nil {
		t.Fatalf("probeBucket contra un S3 válido: %v", err)
	}
	if keys := fake.keys(); len(keys) != 0 {
		t.Fatalf("el probe dejó objetos en el bucket: %v", keys)
	}
}

func TestProbeBucketRejectsMissingBucket(t *testing.T) {
	fake := newFakeS3(t, "media")
	err := probeBucket(presignerFor(t, fake.endpoint, "otro-bucket"))
	if err == nil {
		t.Fatal("un bucket inexistente debería fallar la verificación")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("el error debería nombrar el 404: %v", err)
	}
}

func TestS3PutRejectsSuccessWithoutETag(t *testing.T) {
	server := consoleLikeEndpoint(t)
	backend := newS3Backend(presignerFor(t, server.URL, "media"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := backend.Put(ctx, "images/x.png", []byte("bytes"), "image/png")
	if err == nil {
		t.Fatal("un 200 sin ETag no prueba que el objeto se guardó")
	}
	if !strings.Contains(err.Error(), "ETag") {
		t.Fatalf("error sin pista del ETag: %v", err)
	}
}
