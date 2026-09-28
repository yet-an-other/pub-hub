package s3store_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/storage/s3store"
)

func TestNewRejectsANonLoopbackEndpoint(t *testing.T) {
	_, err := s3store.New(s3store.Config{
		Endpoint:       "https://s3.example.test",
		ArtifactBucket: "pubhub-artifacts",
		MetadataBucket: "pubhub-meta",
		AccessKey:      "access",
		SecretKey:      "secret",
	})
	if err == nil {
		t.Fatal("New accepted a non-loopback endpoint")
	}
}

func TestPutArtifactDoesNotUseAWSChunkedOverTLS(t *testing.T) {
	var contentEncoding, requestPath, authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentEncoding = r.Header.Get("Content-Encoding")
		requestPath = r.URL.EscapedPath()
		authorization = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	store, err := s3store.New(s3store.Config{
		Endpoint:       server.URL,
		ArtifactBucket: "pubhub-artifacts",
		MetadataBucket: "pubhub-meta",
		AccessKey:      "access",
		SecretKey:      "secret",
		HTTPClient:     server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.PutArtifact(context.Background(), "xform/plan.html", strings.NewReader("<title>Plan</title>"), int64(len("<title>Plan</title>")), "text/html"); err != nil {
		t.Fatalf("PutArtifact: %v", err)
	}
	if strings.Contains(strings.ToLower(contentEncoding), "aws-chunked") {
		t.Errorf("PutObject Content-Encoding = %q, must not contain aws-chunked", contentEncoding)
	}
	if requestPath != "/pubhub-artifacts/xform/plan.html" {
		t.Errorf("PutObject path = %q, want path-style bucket prefix", requestPath)
	}
	if !strings.Contains(authorization, "/default/s3/aws4_request") {
		t.Errorf("SigV4 scope = %q, want region default and service s3", authorization)
	}
}
