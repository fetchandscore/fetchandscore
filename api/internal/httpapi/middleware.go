package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

type contextKey int

const userContextKey contextKey = iota

// userFrom returns the authenticated user attached by requireUser.
func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userContextKey).(store.User)
	return u, ok
}

// requireUser rejects unauthenticated requests and attaches the user.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			s.writeError(w, http.StatusUnauthorized, "unauthenticated", "Please sign in.")
			return
		}

		user, err := s.auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrInvalidToken) {
				// The cookie is dead; clear it so the browser stops sending it.
				http.SetCookie(w, s.expireSessionCookie())
				s.writeError(w, http.StatusUnauthorized, "unauthenticated", "Please sign in again.")
				return
			}
			s.fail(w, r, err)
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

// withCORS permits exactly one origin to make credentialed requests.
//
// The frontend is static on GitHub Pages and the API lives on another host, so
// cross-origin is the normal case rather than the exception. A wildcard would
// be invalid here anyway: browsers refuse "*" once credentials are involved.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin != "" && origin == s.cfg.AllowedOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Last-Event-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		// Responses differ by Origin, so caches must not share them.
		w.Header().Add("Vary", "Origin")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// SameSite=Lax stops a cross-site form from carrying the cookie, but a
		// same-site attacker page would still be allowed. Checking Origin on
		// every state-changing request closes that, and costs nothing.
		if isWrite(r.Method) && origin != "" && origin != s.cfg.AllowedOrigin {
			s.writeError(w, http.StatusForbidden, "bad_origin", "Request origin not allowed.")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// withSecurityHeaders sets the headers that are cheap and always correct for a
// JSON API. There is no HTML served here, so a content policy is mostly about
// making a mistake loud rather than defending a rendering surface.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		if s.cfg.SecureCookies {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sessionCookieFor(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:  sessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly keeps the token out of reach of any script, so an XSS
		// anywhere on the site cannot walk off with a session.
		HttpOnly: true,
		// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
		// Not a literal true because a laptop serving over http:// cannot use a
		// Secure cookie at all and sign-in would be untestable. The value is
		// derived from the site's own scheme in cmd/fetchandscore, so anything
		// served over HTTPS gets Secure whatever the flags say.
		Secure: s.cfg.SecureCookies,
		// Lax rather than Strict: the sign-in link arrives from an email
		// client, and Strict would drop the cookie on that first navigation.
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	}
}

func (s *Server) expireSessionCookie() *http.Cookie {
	c := s.sessionCookieFor("", time.Unix(0, 0))
	c.MaxAge = -1
	return c
}

// --- rate limiting ---------------------------------------------------------

// rateLimiter is a fixed-window counter keyed by an arbitrary string.
//
// It guards the endpoints that cost real money or real mail: a token bucket
// per IP and per address stops the sign-in endpoint being turned into a relay.
// In-process is the right scope, because the service is one container.
type rateLimiter struct {
	mu     sync.Mutex
	counts map[string]*window
	limit  int
	period time.Duration
	now    func() time.Time
}

type window struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, period time.Duration) *rateLimiter {
	return &rateLimiter{
		counts: make(map[string]*window),
		limit:  limit,
		period: period,
		now:    time.Now,
	}
}

// allow records an attempt and reports whether it is within the limit.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	w, ok := rl.counts[key]
	if !ok || now.Sub(w.start) >= rl.period {
		rl.counts[key] = &window{count: 1, start: now}
		return true
	}

	w.count++
	return w.count <= rl.limit
}

// sweep discards windows that have expired, so the map does not grow with
// every address that ever tried to sign in.
func (rl *rateLimiter) sweep() {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()
	for key, w := range rl.counts {
		if now.Sub(w.start) >= rl.period {
			delete(rl.counts, key)
		}
	}
}
