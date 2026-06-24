// Package api wires all HTTP handlers into a single http.Server.
// It uses the Go 1.22 stdlib mux (pattern-based routing with {param} support)
// so there are no third-party router dependencies.
package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prem0x01/costblame/internal/api/handlers"
)

// Server is the HTTP server for the REST API and webhook receiver.
type Server struct {
	httpServer *http.Server
}

// New creates a Server with all routes registered. webhooks maps a source name
// to its receiver; each is mounted at POST /webhooks/<name>, so any configured
// deploy source adapter gets an endpoint without the server knowing about it.
func New(
	addr string,
	store handlers.BlameStore,
	webhooks map[string]http.Handler,
) *Server {
	mux := http.NewServeMux()

	blame := handlers.NewBlameHandler(store)

	// REST API
	mux.HandleFunc("GET /blame", blame.ListBlame)
	mux.HandleFunc("GET /blame/{id}", blame.GetBlame)
	mux.HandleFunc("POST /blame/{id}/confirm", blame.ConfirmBlame)
	mux.HandleFunc("POST /blame/{id}/dismiss", blame.DismissBlame)
	mux.HandleFunc("GET /anomalies", blame.ListAnomalies)

	// Webhook receivers — one route per configured deploy source.
	for name, h := range webhooks {
		mux.Handle("POST /webhooks/"+name, h)
		slog.Info("webhook receiver mounted", "path", "/webhooks/"+name)
	}

	// Health check
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	return &Server{
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      loggingMiddleware(mux),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
	}
}

// Start begins listening and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP server listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutCtx)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration", time.Since(start).String(),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
