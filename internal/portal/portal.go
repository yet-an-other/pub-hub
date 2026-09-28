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
	"time"

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
	ln, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(cfg.Socket, socketMode); err != nil {
		return fmt.Errorf("set socket mode: %w", err)
	}

	srv := &http.Server{
		Handler:  routes(),
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

func routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
