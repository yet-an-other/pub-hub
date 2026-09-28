package portal

import (
	"net/http"
	"strings"
	"testing"
)

func TestGetUnknownAndSuffixlessArtifactPathsReturnsNotFound(t *testing.T) {
	handler := newArtifactHandler(t, newMemoryArtifactStore())
	for _, path := range []string{
		"/api/artifacts/xform/notes/missing.html",
		"/api/artifacts/xform/notes/plan",
	} {
		response := artifactRequest(t, handler, http.MethodGet, path, nil, "")
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Errorf("GET %s = %d %s, want 404 not_found", path, response.Code, response.Body)
		}
	}
}
