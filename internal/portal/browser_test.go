package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserAPIUsesOwnerIdentityAndTheSameHandlers(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	for _, tc := range []struct {
		path, email string
		status      int
		label       string
	}{
		{"/ui/api/whoami", "", 401, ""},
		{"/ui/api/whoami", "other@example.test", 403, ""},
		{"/ui/api/whoami", "owner@example.test", 200, `"label":"owner@example.test"`},
		{"/ui/api/artifacts", "owner@example.test", 200, `[]`},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me"+tc.path, nil)
		r.Header.Set("X-Auth-Request-Email", tc.email)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %q: %d, want %d: %s", tc.path, tc.email, w.Code, tc.status, w.Body.String())
		}
		if tc.label != "" && !strings.Contains(w.Body.String(), tc.label) {
			t.Errorf("%s response = %s", tc.path, w.Body.String())
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("unexpected CORS header: %v", w.Header())
		}
	}
}

func TestBrowserMutationRejectsForeignOriginsIncludingSameSite(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	for _, origin := range []string{"https://pub.bdgn.me", "https://elsewhere.example"} {
		r := httptest.NewRequest(http.MethodPatch, "https://hub.bdgn.me/ui/api/projects/xform", strings.NewReader(`{"description":"bad"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("X-Auth-Request-Email", "owner@example.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Origin %q: %d, want 403", origin, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPatch, "https://hub.bdgn.me/ui/api/projects/xform", strings.NewReader(`{"description":"ok"}`))
	r.Header.Set("Origin", "https://hub.bdgn.me")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Auth-Request-Email", "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin PATCH = %d: %s", w.Code, w.Body.String())
	}
}

func TestPlaceholderDoesNotServeArtifactBytes(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me/anything", nil)
	r.Header.Set("X-Auth-Request-Email", "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Catalogue is coming soon") {
		t.Errorf("placeholder = %d: %s", w.Code, w.Body.String())
	}
}
