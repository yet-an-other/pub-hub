// Package auth authenticates machine Publishers with Zitadel PATs.
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
	Active  bool   `json:"active"`
	Subject string `json:"sub"`
}

type cacheEntry struct {
	result    introspectionResult
	expiresAt time.Time
}

// Introspector asks Zitadel whether a PAT is active. Results are cached by a
// SHA-256 digest of the token for 60 seconds; the token itself is never kept
// in the cache.
type Introspector struct {
	endpoint     string
	clientID     string
	clientSecret string
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

	i.mu.Lock()
	i.cache[key] = cacheEntry{result: result, expiresAt: i.now().Add(cacheTTL)}
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

	var result introspectionResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&result); err != nil {
		return introspectionResult{}, fmt.Errorf("%w: invalid introspection response", errIDPUnavailable)
	}
	return result, nil
}

// Publisher is the authenticated machine Publisher identity.
type Publisher struct {
	Subject string
	Label   string
}

type publisherContextKey struct{}

// PublisherFromContext returns the identity attached by Require.
func PublisherFromContext(ctx context.Context) (Publisher, bool) {
	publisher, ok := ctx.Value(publisherContextKey{}).(Publisher)
	return publisher, ok
}

// Authenticator protects the machine Publisher API with bearer-token
// authentication and the configured user-ID allowlist.
type Authenticator struct {
	introspector *Introspector
	allowlist    map[string]string
	log          *slog.Logger
}

// NewAuthenticator constructs an authenticator for the configured Zitadel
// issuer and Publisher allowlist.
func NewAuthenticator(issuerURL, clientID, clientSecret string, allowlist map[string]string, log *slog.Logger) (*Authenticator, error) {
	introspector, err := NewIntrospector(issuerURL, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	allowed := make(map[string]string, len(allowlist))
	for userID, label := range allowlist {
		allowed[userID] = label
	}
	return &Authenticator{introspector: introspector, allowlist: allowed, log: log}, nil
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
		if !result.Active {
			a.reject(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid bearer token", result.Subject)
			return
		}
		label, ok := a.allowlist[result.Subject]
		if !ok {
			a.reject(w, http.StatusForbidden, "forbidden", "Publisher is not allowlisted", result.Subject)
			return
		}

		publisher := Publisher{Subject: result.Subject, Label: label}
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
