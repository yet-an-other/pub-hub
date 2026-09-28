package auth_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/auth"
)

func TestIDPReachableChecksOIDCDiscoveryWithoutFailingStorageReadiness(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Errorf("discovery path = %q", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	authenticator, err := auth.NewAuthenticator(server.URL, "client", "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if !authenticator.IDPReachable(context.Background()) {
		t.Fatal("IDPReachable = false, want true")
	}
	status = http.StatusServiceUnavailable
	if authenticator.IDPReachable(context.Background()) {
		t.Fatal("IDPReachable = true for 503 response, want false")
	}
}
