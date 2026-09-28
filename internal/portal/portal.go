// Package portal runs the Portal: the authenticated UI and publish API on hub.
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yet-an-other/pub-hub/internal/auth"
	"github.com/yet-an-other/pub-hub/internal/config"
)

// socketMode lets only the pubhub user and its group, which nginx joins, reach
// the Portal. That is what lets the Portal trust identity headers later.
const socketMode = 0o660

// shutdownGrace bounds how long in-flight requests may finish after a stop.
const shutdownGrace = 10 * time.Second

// Run loads the config at configPath and serves the Portal on its unix socket
// until ctx is cancelled. It logs JSON to logOut, and logs why before
// returning an error.
func Run(ctx context.Context, configPath string, logOut io.Writer) error {
	log := slog.New(slog.NewJSONHandler(logOut, nil))
	if err := run(ctx, configPath, log); err != nil {
		log.Error("portal stopped", "error", err.Error())
		return err
	}
	return nil
}

func run(ctx context.Context, configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	secret, err := loadCredential(hubAPIClientSecretCredential)
	if err != nil {
		return err
	}
	authenticator, err := auth.NewAuthenticator(cfg.ZitadelIssuerURL, cfg.HubAPIClientID, secret, cfg.Publishers, log)
	if err != nil {
		return fmt.Errorf("configure authentication: %w", err)
	}
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(cfg.Socket, socketMode); err != nil {
		return fmt.Errorf("set socket mode: %w", err)
	}

	srv := &http.Server{
		Handler:  routes(authenticator),
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("portal listening", "socket", cfg.Socket)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("portal shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const hubAPIClientSecretCredential = "hub-api-client-secret"

func loadCredential(name string) (string, error) {
	directory := os.Getenv("CREDENTIALS_DIRECTORY")
	if directory == "" {
		return "", fmt.Errorf("credential %q is unavailable: CREDENTIALS_DIRECTORY is not set", name)
	}
	path := filepath.Join(directory, name)
	secret, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read credential %q: %w", name, err)
	}
	value := strings.TrimSpace(string(secret))
	if value == "" {
		return "", fmt.Errorf("credential %q is empty", name)
	}
	return value, nil
}

func routes(authenticator *auth.Authenticator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	api := http.HandlerFunc(apiRoutes)
	mux.Handle("/api/", authenticator.Require(http.StripPrefix("/api", api)))
	return mux
}

func apiRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/whoami" {
		auth.WriteError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if r.Method != http.MethodGet {
		auth.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	publisher, ok := auth.PublisherFromContext(r.Context())
	if !ok {
		auth.WriteError(w, http.StatusInternalServerError, "internal_error", "publisher identity unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Label string `json:"label"`
	}{Label: publisher.Label})
}
