package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestUploadFilePropagatesS3Outage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<Error><Code>ServiceUnavailable</Code></Error>`))
	}))
	defer server.Close()

	objects, err := New(context.Background(), server.URL, "us-east-1", "bucket", "", "access", "secret", true, false)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "upload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("payload"); err != nil {
		t.Fatal(err)
	}
	if err := objects.UploadFile(context.Background(), file, "object", "text/plain", int64(len("payload"))); err == nil {
		t.Fatal("UploadFile unexpectedly succeeded during an S3 outage")
	}
}

func TestUploadFileHonorsCanceledContext(t *testing.T) {
	reached := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	objects, err := New(context.Background(), server.URL, "us-east-1", "bucket", "", "access", "secret", true, false)
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	file, err := os.CreateTemp(t.TempDir(), "upload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat("x", 32)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := objects.UploadFile(ctx, file, "object", "text/plain", 32); err == nil {
		t.Fatal("UploadFile unexpectedly succeeded with a canceled context")
	}
	select {
	case <-reached:
		t.Fatal("canceled upload reached S3")
	default:
	}
}
