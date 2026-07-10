// Package server exposes Steward's HTTP surface: the GitHub webhook
// receiver plus small health/status endpoints. It is designed to sit behind
// an Nginx reverse proxy that terminates TLS (see clientIP for the
// X-Forwarded-For handling).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alex-savin/steward/internal/config"
	"github.com/alex-savin/steward/internal/dispatch"
)

type Server struct {
	cfg  *config.Config
	log  *slog.Logger
	disp *dispatch.Dispatcher
	http *http.Server
}

func New(cfg *config.Config, log *slog.Logger, disp *dispatch.Dispatcher) *Server {
	s := &Server{cfg: cfg, log: log, disp: disp}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+cfg.Webhook.Path, s.handleWebhook)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /status", s.handleStatus)

	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe() error {
	s.log.Info("http server listening", "addr", s.cfg.Listen, "webhook_path", s.cfg.Webhook.Path)
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStatus reports per-project dispatcher state: what's running, what's
// queued, and the effective concurrency mode.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.disp.Snapshot())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
