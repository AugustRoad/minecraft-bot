package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/AugustRoad/minecraft-bot/internal/events"
)

const (
	// HeaderSecret is the HTTP header carrying the shared VPC authentication token.
	HeaderSecret     = "X-Shared-Secret"
	maxEventBodySize = 64 * 1024
)

// EventHandler handles validated events received from mc-agent.
type EventHandler func(ctx context.Context, ev events.Event)

// Server listens on the private VPC network for events pushed by mc-agent.
type Server struct {
	httpServer *http.Server
	secret     []byte
	handler    EventHandler
	logger     *slog.Logger
}

// NewServer constructs an internal webhook HTTP server.
func NewServer(listenAddr, secret string, handler EventHandler, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		secret:  []byte(secret),
		handler: handler,
		logger:  logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/events", s.handleEvents)

	s.httpServer = &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// Start runs the HTTP server until ctx is canceled, then performs a graceful shutdown.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("webhook server listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	provided := []byte(r.Header.Get(HeaderSecret))
	if len(s.secret) == 0 || len(provided) != len(s.secret) || subtle.ConstantTimeCompare(provided, s.secret) != 1 {
		s.logger.Warn("rejected unauthorized webhook request", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxEventBodySize)
	defer r.Body.Close()

	var ev events.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}
	if !ev.Valid() {
		http.Error(w, "unknown event type", http.StatusBadRequest)
		return
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	if s.handler != nil {
		s.handler(r.Context(), ev)
	}

	w.WriteHeader(http.StatusAccepted)
}
