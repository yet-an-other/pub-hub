// Package portal runs the Portal: the authenticated UI and publish API on hub.
package portal

import (
	"context"
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
	"github.com/yet-an-other/pub-hub/internal/storage/s3store"
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
	accessKey, err := loadCredential(rgwAccessKeyIDCredential)
	if err != nil {
		return err
	}
	secretKey, err := loadCredential(rgwSecretAccessKeyCredential)
	if err != nil {
		return err
	}
	store, err := s3store.New(s3store.Config{
		Endpoint:       cfg.S3Endpoint,
		ArtifactBucket: cfg.ArtifactBucket,
		MetadataBucket: cfg.MetadataBucket,
		AccessKey:      accessKey,
		SecretKey:      secretKey,
	})
	if err != nil {
		return fmt.Errorf("configure S3 storage: %w", err)
	}
	app := newApplication(store, cfg.PublicBaseURL, cfg.SpoolDirectory, log)
	if err := app.prepareSpool(); err != nil {
		return err
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, 5*time.Second)
	app.loadStartup(startupCtx)
	cancelStartup()
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(cfg.Socket, socketMode); err != nil {
		return fmt.Errorf("set socket mode: %w", err)
	}

	srv := &http.Server{
		Handler:  routes(authenticator, app),
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

const (
	hubAPIClientSecretCredential = "hub-api-client-secret"
	rgwAccessKeyIDCredential     = "rgw-access-key-id"
	rgwSecretAccessKeyCredential = "rgw-secret-access-key"
)

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

func routes(authenticator *auth.Authenticator, app *application) http.Handler {
	api := http.StripPrefix("/api", http.HandlerFunc(app.apiRoutes))
	machineAPI := authenticator.Require(api)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/healthz":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/readyz":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			app.readyz(w, r, authenticator)
		default:
			if strings.HasPrefix(r.URL.EscapedPath(), "/api/") {
				machineAPI.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}
