package portal

import (
	"net/http"
	"strings"
	"testing"
)

func TestGetSuffixlessArtifactPathReturnsNotFound(t *testing.T) {
	handler := newArtifactHandler(t, newMemoryArtifactStore())
	if published := multipartRequest(t, handler, "xform/notes/plan.html", "plan.html", "<title>Plan</title>"); published.Code != http.StatusCreated {
		t.Fatalf("publish = %d %s", published.Code, published.Body)
	}
	response := artifactRequest(t, handler, http.MethodGet, "/api/artifacts/xform/notes/plan", nil, "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
		t.Errorf("GET suffixless path = %d %s, want 404 not_found", response.Code, response.Body)
	}
}
