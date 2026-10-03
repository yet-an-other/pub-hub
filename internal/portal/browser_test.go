package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yet-an-other/pub-hub/internal/buildversion"
)

func setBrowserSession(r *http.Request, email string) {
	if email == "" {
		return
	}
	r.Header.Set("X-Auth-Request-User", "user-123")
	r.Header.Set("X-Auth-Request-Email", email)
	r.Header.Set("X-Auth-Request-Access-Token", "browser-token")
}

func TestBrowserAPIUsesAdministratorRoleAndTheSameHandlers(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	for _, tc := range []struct {
		path, email string
		status      int
		label       string
	}{
		{"/ui/api/whoami", "", 401, ""},
		{"/ui/api/whoami", "other@example.test", 200, `"label":"owner","portal_version":"`},
		{"/ui/api/whoami", "owner@example.test", 200, `"label":"owner","portal_version":"`},
		{"/ui/api/artifacts", "owner@example.test", 200, `[]`},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me"+tc.path, nil)
		setBrowserSession(r, tc.email)
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

func TestAPIPrefixesKeepBearerAndBrowserAuthenticationSeparate(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	for _, tc := range []struct {
		path, bearer, email, label string
		status                     int
	}{
		{"/api/whoami", "test-pat", "", `"label":"owner"`, http.StatusOK},
		{"/api/whoami", "", "owner@example.test", "", http.StatusUnauthorized},
		{"/ui/api/whoami", "test-pat", "", "", http.StatusUnauthorized},
		{"/ui/api/whoami", "", "owner@example.test", `"label":"owner"`, http.StatusOK},
	} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me"+tc.path, nil)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		setBrowserSession(r, tc.email)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || (tc.label != "" && !strings.Contains(w.Body.String(), tc.label)) {
			t.Errorf("%s bearer=%t browser=%t: %d %s, want %d %s", tc.path, tc.bearer != "", tc.email != "", w.Code, w.Body.String(), tc.status, tc.label)
		}
	}
}

func TestWhoamiReportsPortalReleaseVersion(t *testing.T) {
	previous := buildversion.Release
	buildversion.Release = "v9.8.7"
	defer func() { buildversion.Release = previous }()
	h := newArtifactHandler(t, newMemoryArtifactStore())
	r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me/ui/api/whoami", nil)
	setBrowserSession(r, "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"label":"owner","portal_version":"v9.8.7"`) {
		t.Fatalf("whoami = %d: %s", w.Code, w.Body.String())
	}
}

func TestBrowserConfigUsesConfiguredReaderHost(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me/ui/api/config", nil)
	setBrowserSession(r, "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"public_base_url":"https://pub.bdgn.me"`) {
		t.Errorf("config = %d: %s", w.Code, w.Body.String())
	}
}

func TestBrowserMutationRejectsForeignOriginsIncludingSameSite(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	for _, origin := range []string{"https://pub.bdgn.me", "https://elsewhere.example"} {
		r := httptest.NewRequest(http.MethodPatch, "https://hub.bdgn.me/ui/api/projects/xform", strings.NewReader(`{"description":"bad"}`))
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "same-site")
		setBrowserSession(r, "owner@example.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("Origin %q: %d, want 403", origin, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPatch, "https://hub.bdgn.me/ui/api/projects/xform", strings.NewReader(`{"description":"ok"}`))
	r.Header.Set("Origin", "https://hub.bdgn.me")
	r.Header.Set("Content-Type", "application/json")
	setBrowserSession(r, "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-origin PATCH = %d: %s", w.Code, w.Body.String())
	}
}

func TestBrowserCatalogueMutations(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	if code := describedPublish(t, h, "xform/plan.html", nil); code != http.StatusCreated {
		t.Fatalf("publish = %d", code)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://hub.bdgn.me/ui/api/"+path, strings.NewReader(body))
		setBrowserSession(r, "owner@example.test")
		r.Header.Set("Origin", "https://hub.bdgn.me")
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct{ path, body, expected string }{
		{"projects/empty", `{"description":"Private"}`, `"description":"Private"`},
		{"artifacts/xform/plan.html", `{"description":"Draft"}`, `"description":"Draft"`},
		{"projects/empty", `{"description":""}`, `"description":""`},
	} {
		w := request(http.MethodPatch, tc.path, tc.body)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.expected) {
			t.Errorf("PATCH %s = %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if w := request(http.MethodGet, "artifacts/xform/plan.html", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"description":"Draft"`) {
		t.Errorf("updated Artifact = %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodDelete, "artifacts/xform/plan.html", ""); w.Code != http.StatusNoContent {
		t.Errorf("DELETE = %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodGet, "artifacts/xform/plan.html", ""); w.Code != http.StatusNotFound {
		t.Errorf("deleted Artifact GET = %d %s", w.Code, w.Body.String())
	}
}

func TestCatalogueShellAndRoutes(t *testing.T) {
	h := newArtifactHandler(t, newMemoryArtifactStore())
	r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me/anything", nil)
	setBrowserSession(r, "owner@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Pub Hub Catalogue") {
		t.Errorf("catalogue = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "srcdoc") || strings.Contains(w.Body.String(), "blob:") {
		t.Fatal("shell contains inline Artifact preview")
	}
	for _, path := range []string{"/", "/nested/client/route"} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me"+path, nil)
		setBrowserSession(r, "owner@example.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Pub Hub Catalogue") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/assets/missing.js", "/ui/api/missing"} {
		r := httptest.NewRequest(http.MethodGet, "https://hub.bdgn.me"+path, nil)
		setBrowserSession(r, "owner@example.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", path, w.Code)
		}
	}
}
