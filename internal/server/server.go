package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// Server owns the HTTP listener and its graceful shutdown.
type Server struct {
	http     *http.Server
	log      *slog.Logger
	shutdown time.Duration
}

// New builds the HTTP server from configuration.
func New(cfg *config.Config, handler http.Handler, log *slog.Logger) *Server {
	return &Server{
		http: &http.Server{
			Addr:              cfg.HTTP.Addr(),
			Handler:           handler,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
			ReadHeaderTimeout: 5 * time.Second, // bounds slowloris-style header stalls
		},
		log:      log,
		shutdown: cfg.HTTP.ShutdownTimeout,
	}
}

// Run serves until ctx is cancelled, then drains in-flight requests.
//
// docs/14 - Infrastructure & Deployment.md §46: stop accepting new requests,
// finish active ones, then exit - so a deploy does not sever a reader
// mid-chapter or a writer mid-save.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.log.Info("http server listening", slog.String("addr", s.http.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		s.log.Info("shutdown signal received; draining requests",
			slog.Duration("timeout", s.shutdown))

		// A fresh context: ctx is already cancelled, so it cannot bound the drain.
		drainCtx, cancel := context.WithTimeout(context.Background(), s.shutdown)
		defer cancel()

		if err := s.http.Shutdown(drainCtx); err != nil {
			s.log.Error("graceful shutdown timed out; forcing close", slog.Any("error", err))
			return s.http.Close()
		}

		s.log.Info("http server stopped cleanly")
		return nil
	}
}
