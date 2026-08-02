// Package httpapi exposes the service over HTTP: a JSON API for the static
// frontend, plus a Server-Sent Events stream for live scoring.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

// Config is everything the server needs from its environment.
type Config struct {
	// AllowedOrigin is the single origin permitted to call the API with
	// credentials. The frontend is served from GitHub Pages on another host,
	// so this is not optional.
	AllowedOrigin string
	// SecureCookies should be false only for local development over plain HTTP.
	SecureCookies bool
}

// Server holds the dependencies of every handler.
type Server struct {
	store *store.Store
	auth  *auth.Service
	hub   *Hub
	cfg   Config
	log   *slog.Logger
	now   func() time.Time

	// linkLimiter caps sign-in link requests. This is the one endpoint that
	// sends mail on an unauthenticated request, so it is the one that could be
	// turned into a relay or used to run up a bill.
	linkLimiter *rateLimiter
}

const (
	// linkRequestLimit and linkRequestWindow allow a handful of attempts —
	// enough for someone who mistypes their address and retries, far short of
	// anything useful for abuse.
	linkRequestLimit  = 5
	linkRequestWindow = 15 * time.Minute
)

func NewServer(st *store.Store, authSvc *auth.Service, hub *Hub, cfg Config, log *slog.Logger) *Server {
	return &Server{
		store:       st,
		auth:        authSvc,
		hub:         hub,
		cfg:         cfg,
		log:         log,
		now:         time.Now,
		linkLimiter: newRateLimiter(linkRequestLimit, linkRequestWindow),
	}
}

// StartHousekeeping runs the periodic cleanup the service needs: expired
// tokens and sessions out of the database, stale windows out of the rate
// limiter. It returns when ctx is cancelled.
func (s *Server) StartHousekeeping(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.store.DeleteExpiredAuth(ctx, s.now()); err != nil {
				s.log.Error("clearing expired credentials", "error", err)
			}
			s.linkLimiter.sweep()
		}
	}
}

// sessionCookie is the name of the cookie carrying the session token.
const sessionCookie = "fns_session"

// Handler builds the routed, middleware-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Authentication.
	mux.HandleFunc("POST /api/auth/request", s.handleAuthRequest)
	mux.HandleFunc("POST /api/auth/verify", s.handleAuthVerify)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("GET /api/me", s.requireUser(s.handleMe))

	// Club administration.
	mux.HandleFunc("POST /api/clubs/{clubID}/invites", s.requireUser(s.handleCreateInvite))

	// Reading.
	mux.HandleFunc("GET /api/dashboard", s.requireUser(s.handleDashboard))
	mux.HandleFunc("GET /api/sessions/{sessionID}", s.requireUser(s.handleSession))
	mux.HandleFunc("GET /api/sessions/{sessionID}/stream", s.requireUser(s.handleStream))
	mux.HandleFunc("GET /api/public/sessions/{sessionID}", s.handlePublicSession)

	// Scoring.
	mux.HandleFunc("POST /api/rounds/{roundID}/start", s.requireUser(s.handleRoundStart))
	mux.HandleFunc("POST /api/rounds/{roundID}/grace", s.requireUser(s.handleRoundGrace))
	mux.HandleFunc("POST /api/rounds/{roundID}/reset", s.requireUser(s.handleRoundReset))
	mux.HandleFunc("POST /api/rounds/{roundID}/confirm", s.requireUser(s.handleRoundConfirm))
	mux.HandleFunc("POST /api/rounds/{roundID}/throws", s.requireUser(s.handleAddThrow))
	mux.HandleFunc("POST /api/rounds/{roundID}/throws/{throwID}/void", s.requireUser(s.handleVoidThrow))

	return s.withSecurityHeaders(s.withCORS(mux))
}

// --- responses -------------------------------------------------------------

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so this can only be logged.
		s.log.Error("writing response", "error", err)
	}
}

// writeError returns a machine-readable code and a message safe to show a user.
func (s *Server) writeError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// fail maps a store or auth error onto a response, logging anything unexpected.
//
// Handlers call this instead of branching, so an internal error can never leak
// a database message to a client.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "not_found", "Not found.")
	case errors.Is(err, store.ErrRoundClosed):
		s.writeError(w, http.StatusConflict, "round_closed",
			"This round is confirmed and can no longer be changed.")
	case errors.Is(err, auth.ErrInvalidToken):
		s.writeError(w, http.StatusUnauthorized, "invalid_token",
			"That link is invalid or has expired.")
	default:
		s.log.Error("request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusInternalServerError, "internal",
			"Something went wrong.")
	}
}

// decodeJSON reads a JSON body, refusing anything oversized or malformed.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	// A scoring payload is a few dozen bytes; 64KB is already absurd headroom
	// and stops a request body from being an attack.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// pathInt reads a positive integer path parameter.
func pathInt(r *http.Request, name string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		return 0, errors.New("invalid " + name)
	}
	return v, nil
}

// --- health ----------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady also proves the database answers, which is what a load balancer
// actually wants to know.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.store.DB().PingContext(ctx); err != nil {
		s.log.Error("readiness check failed", "error", err)
		s.writeError(w, http.StatusServiceUnavailable, "unavailable", "Database unavailable.")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
