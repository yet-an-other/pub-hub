// Package auth authenticates machine Publishers and Portal administrators with Zitadel.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cacheTTL             = 60 * time.Second
	introspectionTimeout = 10 * time.Second
	maxResponseBody      = 1 << 20
)

var errIDPUnavailable = errors.New("identity provider unavailable")

type introspectionResult struct {
	Active    bool
	Subject   string
	Label     string
	Name      string
	Email     string
	Roles     map[string]bool
	ExpiresAt time.Time
}

type cacheEntry struct {
	result    introspectionResult
	expiresAt time.Time
}

// Introspector asks Zitadel whether a token is active. Results are cached by
// the token's SHA-256 digest for at most 60 seconds and never past its expiry;
// active tokens without an expiry claim are not cached. The token itself is
// never kept in the cache.
type Introspector struct {
	endpoint     string
	issuerURL    string
	clientID     string
	clientSecret string
	roleClaim    string
	httpClient   *http.Client
	now          func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cacheEntry
}

// NewIntrospector creates a Zitadel OAuth token-introspection client.
func NewIntrospector(issuerURL, clientID, clientSecret string) (*Introspector, error) {
	issuer, err := url.Parse(issuerURL)
	if err != nil || issuer.Scheme == "" || issuer.Host == "" {
		return nil, fmt.Errorf("invalid Zitadel issuer URL %q", issuerURL)
	}
	if issuer.Scheme != "http" && issuer.Scheme != "https" {
		return nil, fmt.Errorf("invalid Zitadel issuer URL scheme %q", issuer.Scheme)
	}
	if clientID == "" {
		return nil, errors.New("Zitadel client id is required")
	}
	if clientSecret == "" {
		return nil, errors.New("Zitadel client secret is required")
	}

	return &Introspector{
		endpoint:     strings.TrimRight(issuerURL, "/") + "/oauth/v2/introspect",
		issuerURL:    strings.TrimRight(issuerURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: introspectionTimeout},
		now:          time.Now,
		cache:        make(map[[sha256.Size]byte]cacheEntry),
	}, nil
}

func (i *Introspector) inspect(ctx context.Context, token string) (introspectionResult, error) {
	key := sha256.Sum256([]byte(token))
	now := i.now()
	i.mu.Lock()
	if cached, ok := i.cache[key]; ok {
		if now.Before(cached.expiresAt) {
			i.mu.Unlock()
			return cached.result, nil
		}
		delete(i.cache, key)
	}
	i.mu.Unlock()

	result, err := i.request(ctx, token)
	if err != nil {
		return introspectionResult{}, err
	}

	// Without an expiry claim, an active token must be re-introspected on
	// every request: caching it could keep it active past its actual expiry.
	if result.Active && result.ExpiresAt.IsZero() {
		return result, nil
	}
	i.mu.Lock()
	now = i.now()
	expiresAt := now.Add(cacheTTL)
	if !result.ExpiresAt.IsZero() && result.ExpiresAt.Before(expiresAt) {
		expiresAt = result.ExpiresAt
	}
	if now.Before(expiresAt) {
		i.cache[key] = cacheEntry{result: result, expiresAt: expiresAt}
	}
	i.mu.Unlock()
	return result, nil
}

func (i *Introspector) request(ctx context.Context, token string) (introspectionResult, error) {
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return introspectionResult{}, fmt.Errorf("%w: create request", errIDPUnavailable)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(i.clientID, i.clientSecret)

	resp, err := i.httpClient.Do(req)
	if err != nil {
		return introspectionResult{}, fmt.Errorf("%w: request failed", errIDPUnavailable)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return introspectionResult{}, fmt.Errorf("%w: introspection returned %s", errIDPUnavailable, resp.Status)
	}

	var claims map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&claims); err != nil || claims == nil {
		return introspectionResult{}, fmt.Errorf("%w: invalid introspection response", errIDPUnavailable)
	}
	var result introspectionResult
	_ = json.Unmarshal(claims["active"], &result.Active)
	_ = json.Unmarshal(claims["sub"], &result.Subject)
	var exp int64
	if json.Unmarshal(claims["exp"], &exp) == nil && exp > 0 {
		result.ExpiresAt = time.Unix(exp, 0)
		if !i.now().Before(result.ExpiresAt) {
			result.Active = false
		}
	}
	var name, username string
	_ = json.Unmarshal(claims["name"], &name)
	_ = json.Unmarshal(claims["email"], &result.Email)
	result.Name = strings.TrimSpace(name)
	_ = json.Unmarshal(claims["preferred_username"], &username)
	result.Label = result.Subject
	if strings.TrimSpace(username) != "" {
		result.Label = username
	}
	if strings.TrimSpace(name) != "" {
		result.Label = name
	}
	var roles map[string]map[string]json.RawMessage
	if err := json.Unmarshal(claims[i.roleClaim], &roles); err == nil {
		result.Roles = make(map[string]bool)
		for role, organizations := range roles {
			for orgID, rawDomain := range organizations {
				var domain string
				if orgID != "" && json.Unmarshal(rawDomain, &domain) == nil && strings.TrimSpace(domain) != "" {
					result.Roles[role] = true
					break
				}
			}
		}
	}
	return result, nil
}

// reachable reports whether the issuer's OIDC discovery endpoint responds
// successfully. Readiness reports this independently of storage availability.
func (i *Introspector) reachable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.issuerURL+"/.well-known/openid-configuration", nil)
	if err != nil {
		return false
	}
	resp, err := i.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

// Publisher is the authenticated machine Publisher identity.
type Publisher struct {
	Subject      string
	Label        string
	DisplayLabel string
}

type publisherContextKey struct{}

// PublisherFromContext returns the identity attached by Require.
func PublisherFromContext(ctx context.Context) (Publisher, bool) {
	publisher, ok := ctx.Value(publisherContextKey{}).(Publisher)
	return publisher, ok
}

// Authenticator protects the Portal's browser UI and machine Publisher API
// with separate roles in the same Zitadel project.
type Authenticator struct {
	introspector  *Introspector
	publisherRole string
	adminRole     string
	log           *slog.Logger
}

// NewAuthenticator constructs an authenticator for the designated Zitadel roles.
func NewAuthenticator(issuerURL, clientID, clientSecret, projectID, publisherRole, adminRole string, log *slog.Logger) (*Authenticator, error) {
	if projectID == "" || publisherRole == "" || adminRole == "" {
		return nil, errors.New("Zitadel project, publisher role and admin role are required")
	}
	if publisherRole == adminRole {
		return nil, errors.New("Zitadel admin and publisher roles must be different")
	}
	introspector, err := NewIntrospector(issuerURL, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	introspector.roleClaim = "urn:zitadel:iam:org:project:" + projectID + ":roles"
	return &Authenticator{introspector: introspector, publisherRole: publisherRole, adminRole: adminRole, log: log}, nil
}

// IDPReachable reports whether the configured Zitadel issuer is reachable.
func (a *Authenticator) IDPReachable(ctx context.Context) bool {
	return a.introspector.reachable(ctx)
}

// RequireAdmin trusts the browser session identity and access token forwarded
// by nginx from oauth2-proxy's auth subrequest. Only nginx may reach the Portal
// socket; nginx must overwrite these headers on every Portal location.
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subject := r.Header.Get("X-Auth-Request-User")
		email := r.Header.Get("X-Auth-Request-Email")
		token := r.Header.Get("X-Auth-Request-Access-Token")
		if subject == "" || email == "" || token == "" {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "browser session required")
			return
		}
		result, err := a.introspector.inspect(r.Context(), token)
		if err != nil {
			WriteError(w, http.StatusServiceUnavailable, "idp_unavailable", "identity provider unavailable")
			return
		}
		if !result.Active || result.Subject == "" || result.Subject != subject {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid browser session")
			return
		}
		if !result.Roles[a.adminRole] {
			WriteError(w, http.StatusForbidden, "forbidden", "Portal administrator role required")
			return
		}
		// Prefer a person's name, then an actual email address. Some OIDC
		// sessions put the opaque subject in oauth2-proxy's email header.
		label := result.Name
		if label == subject {
			label = ""
		}
		if label == "" && strings.Contains(result.Email, "@") {
			label = result.Email
		}
		if label == "" && strings.Contains(email, "@") {
			label = email
		}
		displayLabel := label
		if displayLabel == "" {
			displayLabel = "Administrator"
		}
		if strings.Contains(email, "@") {
			label = email // Preserve the browser Publisher label for existing accounts.
		} else if label == "" {
			label = result.Label // Keep a unique audit label if no readable claim exists.
		}
		publisher := Publisher{Subject: subject, Label: label, DisplayLabel: displayLabel}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), publisherContextKey{}, publisher)))
	})
}

// Require authenticates a machine Publisher and adds its identity to the
// request context before invoking next. Cookies and identity headers are not
// consulted.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.reject(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token", "")
			return
		}

		result, err := a.introspector.inspect(r.Context(), token)
		if err != nil {
			a.reject(w, http.StatusServiceUnavailable, "idp_unavailable", "identity provider unavailable", "")
			return
		}
		if !result.Active || strings.TrimSpace(result.Subject) == "" {
			a.reject(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token", result.Subject)
			return
		}
		if !result.Roles[a.publisherRole] {
			a.reject(w, http.StatusForbidden, "forbidden", "Publisher role required", result.Subject)
			return
		}

		publisher := Publisher{Subject: result.Subject, Label: result.Label}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), publisherContextKey{}, publisher)))
	})
}

func bearerToken(value string) (string, bool) {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
}

func (a *Authenticator) reject(w http.ResponseWriter, status int, code, message, subject string) {
	attrs := []any{"reason", code}
	if subject != "" {
		attrs = append(attrs, "sub", subject)
	}
	a.log.Warn("authentication failed", attrs...)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	WriteError(w, status, code, message)
}

// WriteError writes the stable API error envelope.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}
