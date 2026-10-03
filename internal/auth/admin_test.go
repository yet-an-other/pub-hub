package auth

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func adminRequest(handler http.Handler, subject, email, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/ui/api/whoami", nil)
	r.Header.Set("X-Auth-Request-User", subject)
	r.Header.Set("X-Auth-Request-Email", email)
	r.Header.Set("X-Auth-Request-Access-Token", token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRequireAdminNeedsTheSessionTokenAndMatchingSubject(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "human-123", admin: true}
	var logs bytes.Buffer
	a, _ := newAuthenticatorFixture(t, fixture, &logs)
	handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PublisherFromContext(r.Context())
		if !ok || p.Subject != "human-123" || p.Label != r.Header.Get("X-Auth-Request-Email") {
			t.Errorf("browser Publisher = %+v, present = %t", p, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name, subject, email, token string
		status                      int
	}{
		{"another admin", "human-123", "another@example.test", "browser-token", 204},
		{"missing token", "human-123", "human@example.test", "", 401},
		{"missing subject", "", "human@example.test", "browser-token", 401},
		{"missing email", "human-123", "", "browser-token", 401},
		{"wrong subject", "attacker", "human@example.test", "browser-token", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := adminRequest(handler, tc.subject, tc.email, tc.token)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.gotToken != "browser-token" {
		t.Errorf("introspected token = %q", fixture.gotToken)
	}
	if strings.Contains(logs.String(), "browser-token") {
		t.Fatal("browser access token logged")
	}
}

func TestRequireAdminUsesReadableIdentityWhenProxyEmailIsSubject(t *testing.T) {
	for _, tc := range []struct {
		name, claims, label, display string
	}{
		{"name", `"name":"Ada Lovelace"`, "Ada Lovelace", "Ada Lovelace"},
		{"email", `"email":"ada@example.test"`, "ada@example.test", "ada@example.test"},
		{"name is subject", `"name":"human-123"`, "human-123", "Administrator"},
		{"missing claims", `"preferred_username":"human-123"`, "human-123", "Administrator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &introspectionFixture{response: `{"active":true,"sub":"human-123",` + tc.claims + `,"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"org-456":"example.test"}}}`}
			var logs bytes.Buffer
			a, _ := newAuthenticatorFixture(t, fixture, &logs)
			handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, _ := PublisherFromContext(r.Context())
				if p.Label != tc.label || p.DisplayLabel != tc.display {
					t.Errorf("identity = %+v, want %q / %q", p, tc.label, tc.display)
				}
			}))
			if w := adminRequest(handler, "human-123", "human-123", "browser-token"); w.Code != 200 {
				t.Errorf("status = %d", w.Code)
			}
		})
	}
}

func TestRequireAdminOnlyAcceptsTheProjectQualifiedRoleInAnyOrganization(t *testing.T) {
	for _, tc := range []struct {
		name, claims string
		status       int
	}{
		{"another organization", `"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"other-org":"example.test"}}`, 204},
		{"publisher only", `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":"example.test"}}`, 403},
		{"wrong project", `"urn:zitadel:iam:org:project:other:roles":{"hub-admin":{"org-456":"example.test"}}`, 403},
		{"bare role", `"urn:zitadel:iam:org:project:roles":{"hub-admin":{"org-456":"example.test"}}`, 403},
		{"empty domain", `"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"org-456":""}}`, 403},
		{"inactive", `"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"org-456":"example.test"}}`, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active := tc.name != "inactive"
			fixture := &introspectionFixture{response: fmt.Sprintf(`{"active":%t,"sub":"human-123",%s}`, active, tc.claims)}
			var logs bytes.Buffer
			a, _ := newAuthenticatorFixture(t, fixture, &logs)
			handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			w := adminRequest(handler, "human-123", "human@example.test", "browser-token")
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
		})
	}
}

func TestRequireAdminRefreshesRolesAfterCacheExpiryAndFailsClosedOnOutage(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "human-123", admin: true}
	var logs bytes.Buffer
	a, _ := newAuthenticatorFixture(t, fixture, &logs)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.introspector.now = func() time.Time { return now }
	handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	request := func() *httptest.ResponseRecorder {
		return adminRequest(handler, "human-123", "human@example.test", "browser-token")
	}
	if got := request().Code; got != 204 {
		t.Fatalf("granted = %d", got)
	}
	fixture.mu.Lock()
	fixture.admin = false
	fixture.mu.Unlock()
	if got := request().Code; got != 204 {
		t.Errorf("cached grant = %d", got)
	}
	now = now.Add(cacheTTL)
	if got := request().Code; got != 403 {
		t.Errorf("removed grant = %d", got)
	}
	fixture.mu.Lock()
	fixture.unavailable = true
	fixture.mu.Unlock()
	now = now.Add(cacheTTL)
	w := request()
	if w.Code != 503 || errorCode(t, w) != "idp_unavailable" {
		t.Errorf("expired authorization with outage = %d %s", w.Code, w.Body.String())
	}
}

func TestConfiguredRoleNamesStayIndependent(t *testing.T) {
	fixture := &introspectionFixture{response: `{"active":true,"sub":"human-123","urn:zitadel:iam:org:project:project-123:roles":{"operators":{"another-org":"example.test"}}}`}
	var logs bytes.Buffer
	_, idp := newAuthenticatorFixture(t, fixture, &logs)
	a, err := NewAuthenticator(idp.URL, "hub-api-client", "client-secret", "project-123", "writers", "operators", nil)
	if err != nil {
		t.Fatal(err)
	}
	admin := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	if got := adminRequest(admin, "human-123", "human@example.test", "browser-token").Code; got != 204 {
		t.Errorf("configured admin role = %d", got)
	}
	machine := a.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	r.Header.Set("Authorization", "Bearer machine-token")
	w := httptest.NewRecorder()
	machine.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("admin role used on machine API = %d", w.Code)
	}
}

func TestActiveTokenWithoutUsableExpiryIsNeverCached(t *testing.T) {
	for _, exp := range []string{"", `,"exp":"unknown"`} {
		fixture := &introspectionFixture{response: `{"active":true,"sub":"human-123"` + exp + `,"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"org-456":"example.test"}}}`}
		var logs bytes.Buffer
		a, _ := newAuthenticatorFixture(t, fixture, &logs)
		handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		if got := adminRequest(handler, "human-123", "human@example.test", "browser-token").Code; got != 204 {
			t.Fatalf("active token without usable expiry = %d", got)
		}
		fixture.mu.Lock()
		fixture.response = `{"active":false,"sub":"human-123"}`
		fixture.mu.Unlock()
		if got := adminRequest(handler, "human-123", "human@example.test", "browser-token").Code; got != 401 {
			t.Errorf("expired token without usable expiry = %d, want 401", got)
		}
		fixture.mu.Lock()
		requests := fixture.requests
		fixture.mu.Unlock()
		if requests != 2 {
			t.Errorf("introspection requests = %d, want 2", requests)
		}
	}
}

func TestIntrospectionCacheNeverOutlivesTokenExpiry(t *testing.T) {
	fixture := &introspectionFixture{response: `{"active":true,"sub":"human-123","exp":1767225620,"urn:zitadel:iam:org:project:project-123:roles":{"hub-admin":{"org-456":"example.test"}}}`}
	var logs bytes.Buffer
	a, _ := newAuthenticatorFixture(t, fixture, &logs)
	now := time.Unix(1767225600, 0)
	a.introspector.now = func() time.Time { return now }
	handler := a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	if got := adminRequest(handler, "human-123", "human@example.test", "browser-token").Code; got != 204 {
		t.Fatalf("active token = %d", got)
	}
	now = now.Add(20 * time.Second)
	if got := adminRequest(handler, "human-123", "human@example.test", "browser-token").Code; got != 401 {
		t.Errorf("expired token = %d, want 401", got)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 2 {
		t.Errorf("introspection requests = %d, want 2", fixture.requests)
	}
}
