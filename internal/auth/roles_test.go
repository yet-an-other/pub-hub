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

// Exercise the public bearer middleware with claim bodies Zitadel can return.
func TestRequireOnlyTrustsTheExactProjectOrganizationRole(t *testing.T) {
	const grant = `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":"example.test"}}`
	cases := []struct {
		name, claims string
		status       int
	}{
		{"exact grant", grant, http.StatusNoContent},
		{"another project", `"urn:zitadel:iam:org:project:other:roles":{"publisher":{"org-456":"example.test"}}`, http.StatusForbidden},
		{"another organization", `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"other":"example.test"}}`, http.StatusForbidden},
		{"unqualified role", `"urn:zitadel:iam:org:project:roles":{"publisher":{"org-456":"example.test"}}`, http.StatusForbidden},
		{"scope and audience", `"scope":"openid urn:zitadel:iam:org:project:project-123:roles:publisher","aud":["project-123"]`, http.StatusForbidden},
		{"no role", `"name":"Test Account"`, http.StatusForbidden},
		{"missing organization leaf", `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{}}`, http.StatusForbidden},
		{"wrong leaf type", `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":true}}`, http.StatusForbidden},
		{"empty domain", `"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":""}}`, http.StatusForbidden},
		{"malformed roles", `"urn:zitadel:iam:org:project:project-123:roles":["publisher"]`, http.StatusForbidden},
		{"malformed subject", `"sub":123,` + grant, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Duplicate sub is only used for malformed-sub case; the last claim wins.
			base := `"active":true,"sub":"subject-1","name":"Test Account"`
			if tc.name == "malformed subject" {
				base = `"active":true`
			}
			fixture := &introspectionFixture{response: fmt.Sprintf(`{%s,%s}`, base, tc.claims)}
			var logs bytes.Buffer
			a, _ := newAuthenticatorFixture(t, fixture, &logs)
			handler := a.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
			r.Header.Set("Authorization", "Bearer secret")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Errorf("status = %d, want %d", w.Code, tc.status)
			}
			if strings.Contains(logs.String(), "secret") || strings.Contains(w.Body.String(), "secret") {
				t.Error("token leaked")
			}
		})
	}
}

func TestRequireRefreshesRolesAndLabelsAfterCacheExpiry(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "subject-1", role: true, name: "Old Name"}
	var logs bytes.Buffer
	a, _ := newAuthenticatorFixture(t, fixture, &logs)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.introspector.now = func() time.Time { return now }
	handler := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PublisherFromContext(r.Context())
		fmt.Fprint(w, p.Label)
	}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
		r.Header.Set("Authorization", "Bearer same-pat")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request().Body.String(); got != "Old Name" {
		t.Fatalf("initial label = %q", got)
	}
	fixture.mu.Lock()
	fixture.name = "New Name"
	fixture.role = false
	fixture.mu.Unlock()
	if got := request().Body.String(); got != "Old Name" {
		t.Errorf("cached label = %q", got)
	}
	now = now.Add(cacheTTL)
	if got := request().Code; got != http.StatusForbidden {
		t.Fatalf("after removal = %d, want 403", got)
	}
	fixture.mu.Lock()
	fixture.role = true
	fixture.mu.Unlock()
	now = now.Add(cacheTTL)
	if got := request().Body.String(); got != "New Name" {
		t.Errorf("after reassignment = %q, want New Name", got)
	}
}

func TestRequireUsesUsernameThenSubjectWhenNameIsMissing(t *testing.T) {
	for _, tc := range []struct{ name, username, want string }{
		{"", "account@example.test", "account@example.test"},
		{"", "", "subject-1"},
		{"Display Name", "account@example.test", "Display Name"},
	} {
		fixture := &introspectionFixture{active: true, subject: "subject-1", name: tc.name, username: tc.username, role: true}
		var logs bytes.Buffer
		a, _ := newAuthenticatorFixture(t, fixture, &logs)
		handler := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := PublisherFromContext(r.Context())
			fmt.Fprint(w, p.Label)
		}))
		r := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
		r.Header.Set("Authorization", "Bearer pat")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("label = %q, status = %d; want %q, 200", w.Body.String(), w.Code, tc.want)
		}
	}
}
