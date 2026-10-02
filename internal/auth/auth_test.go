package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type introspectionFixture struct {
	mu          sync.Mutex
	active      bool
	subject     string
	name        string
	username    string
	role        bool
	response    string
	unavailable bool
	requests    int
	gotToken    string
	gotUsername string
}

func (f *introspectionFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/oauth/v2/introspect" {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	f.gotToken = r.Form.Get("token")
	f.gotUsername, _, _ = r.BasicAuth()
	if f.unavailable {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.response != "" {
		fmt.Fprint(w, f.response)
		return
	}
	fmt.Fprintf(w, `{"active":%t,"sub":%q,"name":%q,"preferred_username":%q`, f.active, f.subject, f.name, f.username)
	if f.role {
		fmt.Fprint(w, `,"urn:zitadel:iam:org:project:project-123:roles":{"publisher":{"org-456":"example.test"}}`)
	}
	fmt.Fprint(w, `}`)
}

func newAuthenticatorFixture(t *testing.T, fixture *introspectionFixture, logs *bytes.Buffer) (*Authenticator, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	authenticator, err := NewAuthenticator(server.URL, "hub-api-client", "client-secret", "project-123", "org-456", "publisher", logger)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return authenticator, server
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v; body=%q", err, response.Body.String())
	}
	return body.Error.Code
}

func TestRequireAcceptsARoleGrantedPATAndAddsPublisherIdentity(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "user-123", name: "owner", role: true}
	var logs bytes.Buffer
	authenticator, _ := newAuthenticatorFixture(t, fixture, &logs)

	handler := authenticator.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publisher, ok := PublisherFromContext(r.Context())
		if !ok {
			t.Error("PublisherFromContext returned no Publisher")
			return
		}
		fmt.Fprint(w, publisher.Label)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://hub.bdgn.me/api/whoami", nil)
	request.Header.Set("Authorization", "Bearer pat-secret-value")
	request.AddCookie(&http.Cookie{Name: "session", Value: "must-be-ignored"})
	request.Header.Set("X-Auth-Request-Email", "forged@example.test")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	if got := response.Body.String(); got != "owner" {
		t.Errorf("identity label = %q, want owner", got)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 1 {
		t.Errorf("introspection requests = %d, want 1", fixture.requests)
	}
	if fixture.gotToken != "pat-secret-value" {
		t.Errorf("introspection token = %q", fixture.gotToken)
	}
	if fixture.gotUsername != "hub-api-client" {
		t.Errorf("introspection username = %q, want hub-api-client", fixture.gotUsername)
	}
}

func TestRequireRejectsMissingOrMalformedBearerCredentials(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "user-123"}
	var logs bytes.Buffer
	authenticator, _ := newAuthenticatorFixture(t, fixture, &logs)
	handler := authenticator.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("protected handler ran")
	}))

	for _, authorization := range []string{"", "Basic pat", "Bearer", "Bearer one two"} {
		t.Run(fmt.Sprintf("authorization=%q", authorization), func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://hub.bdgn.me/api/whoami", nil)
			request.Header.Set("Authorization", authorization)
			request.AddCookie(&http.Cookie{Name: "session", Value: "ignored"})
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", response.Code)
			}
			if got := errorCode(t, response); got != "unauthenticated" {
				t.Errorf("error code = %q, want unauthenticated", got)
			}
		})
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 0 {
		t.Errorf("malformed credentials caused %d introspection requests", fixture.requests)
	}
}

func TestRequireDistinguishesInactiveAndUnauthorizedPublishers(t *testing.T) {
	cases := []struct {
		name       string
		active     bool
		subject    string
		role       bool
		wantStatus int
		wantCode   string
	}{
		{name: "inactive", active: false, subject: "user-123", role: true, wantStatus: http.StatusUnauthorized, wantCode: "unauthenticated"},
		{name: "no role", active: true, subject: "user-456", wantStatus: http.StatusForbidden, wantCode: "forbidden"},
		{name: "missing subject", active: true, subject: "", role: true, wantStatus: http.StatusUnauthorized, wantCode: "unauthenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &introspectionFixture{active: tc.active, subject: tc.subject, role: tc.role}
			var logs bytes.Buffer
			authenticator, _ := newAuthenticatorFixture(t, fixture, &logs)
			handler := authenticator.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("protected handler ran")
			}))
			request := httptest.NewRequest(http.MethodGet, "http://hub.bdgn.me/api/whoami", nil)
			request.Header.Set("Authorization", "Bearer pat")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", response.Code, tc.wantStatus)
			}
			if got := errorCode(t, response); got != tc.wantCode {
				t.Errorf("error code = %q, want %q", got, tc.wantCode)
			}
			if tc.subject != "" && !strings.Contains(logs.String(), tc.subject) {
				t.Errorf("logs = %q, want subject %q", logs.String(), tc.subject)
			}
			if strings.Contains(logs.String(), "Bearer pat") || strings.Contains(logs.String(), "pat") {
				t.Errorf("logs contain the token: %q", logs.String())
			}
		})
	}
}

func TestRequireReturnsIDPUnavailableWhenIntrospectionCannotBeCompleted(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "user-123", role: true, unavailable: true}
	var logs bytes.Buffer
	authenticator, _ := newAuthenticatorFixture(t, fixture, &logs)
	handler := authenticator.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("protected handler ran")
	}))
	request := httptest.NewRequest(http.MethodGet, "http://hub.bdgn.me/api/whoami", nil)
	request.Header.Set("Authorization", "Bearer pat")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", response.Code)
	}
	if got := errorCode(t, response); got != "idp_unavailable" {
		t.Errorf("error code = %q, want idp_unavailable", got)
	}
}

func TestRequireUsesARecentCachedResultDuringAnIDPOutageAndExpiresIt(t *testing.T) {
	fixture := &introspectionFixture{active: true, subject: "user-123", role: true}
	var logs bytes.Buffer
	authenticator, _ := newAuthenticatorFixture(t, fixture, &logs)
	currentTime := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	authenticator.introspector.now = func() time.Time { return currentTime }
	handler := authenticator.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://hub.bdgn.me/api/whoami", nil)
		r.Header.Set("Authorization", "Bearer pat")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}

	if response := request(); response.Code != http.StatusNoContent {
		t.Fatalf("initial status = %d, want 204", response.Code)
	}
	fixture.mu.Lock()
	fixture.unavailable = true
	fixture.mu.Unlock()
	if response := request(); response.Code != http.StatusNoContent {
		t.Fatalf("cached status = %d, want 204", response.Code)
	}
	fixture.mu.Lock()
	requestsAfterCacheHit := fixture.requests
	fixture.mu.Unlock()
	if requestsAfterCacheHit != 1 {
		t.Errorf("requests after cached result = %d, want 1", requestsAfterCacheHit)
	}

	currentTime = currentTime.Add(cacheTTL)
	response := request()
	if response.Code != http.StatusServiceUnavailable {
		t.Errorf("expired status = %d, want 503", response.Code)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.requests != 2 {
		t.Errorf("requests after expiry = %d, want 2", fixture.requests)
	}
}

func TestNewIntrospectorRejectsANonAbsoluteIssuer(t *testing.T) {
	_, err := NewIntrospector("zitadel", "client", "secret")
	if err == nil {
		t.Fatal("NewIntrospector succeeded, want an error")
	}
}
