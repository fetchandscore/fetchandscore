package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

// --- authentication --------------------------------------------------------

type authRequestBody struct {
	Email string `json:"email"`
}

// handleAuthRequest emails a sign-in link.
//
// It answers 202 whatever happens, including for an address nobody invited.
// Any other behaviour would make this endpoint a way to ask "does this person
// have an account?".
func (s *Server) handleAuthRequest(w http.ResponseWriter, r *http.Request) {
	var body authRequestBody
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Expected an email address.")
		return
	}
	if body.Email == "" {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Expected an email address.")
		return
	}

	if !s.linkLimiter.allow(clientIP(r)) || !s.linkLimiter.allow("email:"+body.Email) {
		s.writeError(w, http.StatusTooManyRequests, "rate_limited",
			"Too many sign-in attempts. Try again in a few minutes.")
		return
	}

	if err := s.auth.RequestLink(r.Context(), body.Email); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]bool{"sent": true})
}

type authVerifyBody struct {
	Token string `json:"token"`
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	var body authVerifyBody
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Expected a token.")
		return
	}

	token, user, err := s.auth.Verify(r.Context(), body.Token, r.UserAgent())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	http.SetCookie(w, s.sessionCookieFor(token, s.now().Add(auth.SessionLifetime)))
	s.writeJSON(w, http.StatusOK, userResponse(user))
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), cookie.Value); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.SetCookie(w, s.expireSessionCookie())
	s.writeJSON(w, http.StatusOK, map[string]bool{"signed_out": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	s.writeJSON(w, http.StatusOK, userResponse(user))
}

func userResponse(u store.User) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "name": u.Name}
}

// clientIP prefers the address the proxy reports, falling back to the socket.
//
// Cloudflare Tunnel terminates the connection, so RemoteAddr is the tunnel,
// not the caller. This is only used for rate limiting, where a spoofed header
// costs an attacker nothing anyway — it is a courtesy limit, not a control.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// --- club administration ---------------------------------------------------

type inviteBody struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	clubID, err := pathInt(r, "clubID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid club.")
		return
	}
	if !s.requireRole(w, r, clubID, user.ID, store.RoleCaptain) {
		return
	}

	var body inviteBody
	if err := decodeJSON(w, r, &body); err != nil || body.Email == "" {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Expected an email address and role.")
		return
	}

	role := store.Role(body.Role)
	if _, known := map[store.Role]bool{
		store.RoleMember: true, store.RoleScorekeeper: true,
		store.RoleCaptain: true, store.RoleAdmin: true,
	}[role]; !known {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Unknown role.")
		return
	}

	link, err := s.auth.Invite(r.Context(), clubID, body.Email, role, &user.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]string{"invite_url": link})
}

// requireRole checks club membership and authority, writing the response and
// returning false if the caller may not proceed.
//
// A non-member gets 404 rather than 403: whether a club exists is not
// something an outsider needs to learn.
func (s *Server) requireRole(w http.ResponseWriter, r *http.Request, clubID, userID int64, min store.Role) bool {
	role, err := s.store.MemberRole(r.Context(), clubID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return false
		}
		s.fail(w, r, err)
		return false
	}
	if !role.AtLeast(min) {
		s.writeError(w, http.StatusForbidden, "forbidden", "You do not have permission to do that.")
		return false
	}
	return true
}

// --- reading ---------------------------------------------------------------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	dash, err := s.store.Dashboard(r.Context(), user.ID, s.now())
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"active":   summaries(dash.Active),
		"upcoming": summaries(dash.Upcoming),
		"past":     summaries(dash.Past),
	})
}

func summaries(xs []store.SessionSummary) []map[string]any {
	// A nil slice marshals to null; the frontend wants an array to iterate.
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, map[string]any{
			"id":         x.ID,
			"club_id":    x.ClubID,
			"club_name":  x.ClubName,
			"name":       x.Name,
			"starts_at":  x.StartsAt.Format(time.RFC3339),
			"status":     x.Status,
			"team_count": x.TeamCount,
		})
	}
	return out
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())

	sessionID, err := pathInt(r, "sessionID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid session.")
		return
	}

	view, err := s.store.SessionView(r.Context(), sessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !s.requireRole(w, r, view.Session.ClubID, user.ID, store.RoleMember) {
		return
	}

	s.writeJSON(w, http.StatusOK, sessionResponse(view, true))
}

// handlePublicSession serves confirmed results without authentication, so a
// handler can share a link. Rounds still in progress are withheld: live
// scoring belongs to the people at the field.
func (s *Server) handlePublicSession(w http.ResponseWriter, r *http.Request) {
	sessionID, err := pathInt(r, "sessionID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid session.")
		return
	}

	view, err := s.store.SessionView(r.Context(), sessionID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Public results are immutable once confirmed, but a session gains rounds
	// as the evening goes on, so only a short cache is safe.
	w.Header().Set("Cache-Control", "public, max-age=60")
	s.writeJSON(w, http.StatusOK, sessionResponse(view, false))
}

func sessionResponse(v store.SessionView, includeUnconfirmed bool) map[string]any {
	teams := make([]map[string]any, 0, len(v.Teams))
	for _, t := range v.Teams {
		rounds := make([]map[string]any, 0, len(t.Rounds))
		for _, rd := range t.Rounds {
			if !includeUnconfirmed && rd.Status != store.RoundConfirmed {
				continue
			}
			rounds = append(rounds, roundResponse(rd))
		}
		teams = append(teams, map[string]any{
			"season_entry_id": t.SeasonEntryID,
			"team_name":       t.TeamName,
			"handler_name":    t.HandlerName,
			"dog_name":        t.DogName,
			"division":        t.Division.String(),
			"tiny":            t.Tiny,
			"roller":          t.Roller,
			"running_order":   t.RunningOrder,
			"rounds":          rounds,
		})
	}

	return map[string]any{
		"id":        v.Session.ID,
		"club_id":   v.Session.ClubID,
		"club_name": v.ClubName,
		"name":      v.Session.Name,
		"starts_at": v.Session.StartsAt.Format(time.RFC3339),
		"status":    v.Session.Status,
		"format": map[string]any{
			"name":             v.Format.Name,
			"round_seconds":    v.Format.RoundSeconds,
			"rounds_per_week":  v.Format.RoundsPerWeek,
			"scored_throw_cap": v.Format.ScoredThrowCap,
			"cue_seconds":      v.Format.CueSeconds,
		},
		"teams": teams,
	}
}

func roundResponse(r store.Round) map[string]any {
	out := map[string]any{
		"id":           r.ID,
		"number":       r.Number,
		"status":       string(r.Status),
		"total_points": r.TotalX2.Points(),
	}
	// The client derives its clock from the server's start stamp, so a
	// spectator's timer tracks the scorekeeper's.
	if r.StartedAt != nil {
		out["started_at"] = r.StartedAt.Format(time.RFC3339Nano)
	}
	if r.ConfirmedAt != nil {
		out["confirmed_at"] = r.ConfirmedAt.Format(time.RFC3339Nano)
	}
	return out
}

func throwResponse(t store.Throw) map[string]any {
	return map[string]any{
		"id":          t.ID,
		"client_id":   t.ClientID,
		"zone":        t.Zone.String(),
		"air":         t.Air,
		"void":        t.Void,
		"recorded_at": t.RecordedAt.Format(time.RFC3339Nano),
	}
}

// --- scoring ---------------------------------------------------------------

// roundAccess resolves a round, checks the caller may score it, and returns
// the round's session for publishing events.
func (s *Server) roundAccess(w http.ResponseWriter, r *http.Request) (roundID, sessionID int64, ok bool) {
	user, _ := userFrom(r.Context())

	roundID, err := pathInt(r, "roundID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid round.")
		return 0, 0, false
	}

	clubID, sessionID, err := s.store.RoundLocation(r.Context(), roundID)
	if err != nil {
		s.fail(w, r, err)
		return 0, 0, false
	}
	if !s.requireRole(w, r, clubID, user.ID, store.RoleScorekeeper) {
		return 0, 0, false
	}
	return roundID, sessionID, true
}

func (s *Server) handleRoundStart(w http.ResponseWriter, r *http.Request) {
	user, _ := userFrom(r.Context())
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	at := s.now()
	if err := s.store.StartRound(r.Context(), roundID, user.ID, at); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishRound(r, sessionID, roundID, "round.started")
	s.respondWithRound(w, r, roundID)
}

func (s *Server) handleRoundGrace(w http.ResponseWriter, r *http.Request) {
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	if err := s.store.EnterGrace(r.Context(), roundID, s.now()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishRound(r, sessionID, roundID, "round.grace")
	s.respondWithRound(w, r, roundID)
}

func (s *Server) handleRoundReset(w http.ResponseWriter, r *http.Request) {
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	if err := s.store.ResetRound(r.Context(), roundID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishRound(r, sessionID, roundID, "round.reset")
	s.respondWithRound(w, r, roundID)
}

func (s *Server) handleRoundConfirm(w http.ResponseWriter, r *http.Request) {
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	if err := s.store.ConfirmRound(r.Context(), roundID, s.now()); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishRound(r, sessionID, roundID, "round.confirmed")
	s.respondWithRound(w, r, roundID)
}

type throwBody struct {
	ClientID   string `json:"client_id"`
	Zone       string `json:"zone"`
	Air        bool   `json:"air"`
	RecordedAt string `json:"recorded_at"`
}

func (s *Server) handleAddThrow(w http.ResponseWriter, r *http.Request) {
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	var body throwBody
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Malformed throw.")
		return
	}
	if body.ClientID == "" {
		// Without this the write is not idempotent, and a retry would score
		// twice. Refuse rather than silently accept an unsafe write.
		s.writeError(w, http.StatusBadRequest, "bad_request", "A client_id is required.")
		return
	}

	zone, err := scoring.ParseZone(body.Zone)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Unknown zone.")
		return
	}

	// The client stamps the tap so a queue draining after a dropout keeps its
	// true order. A missing or unparseable stamp falls back to arrival time.
	recordedAt := s.now()
	if body.RecordedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, body.RecordedAt); err == nil {
			recordedAt = parsed
		}
	}

	throw, err := s.store.AddThrow(r.Context(), roundID, store.NewThrow{
		ClientID:   body.ClientID,
		Zone:       zone,
		Air:        body.Air,
		RecordedAt: recordedAt,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	round, err := s.store.Round(r.Context(), roundID)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	s.hub.Publish(sessionID, "throw.added", map[string]any{
		"round": roundResponse(round),
		"throw": throwResponse(throw),
	})
	s.writeJSON(w, http.StatusOK, map[string]any{
		"round": roundResponse(round),
		"throw": throwResponse(throw),
	})
}

func (s *Server) handleVoidThrow(w http.ResponseWriter, r *http.Request) {
	roundID, sessionID, ok := s.roundAccess(w, r)
	if !ok {
		return
	}

	throwID, err := pathInt(r, "throwID")
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "bad_request", "Invalid throw.")
		return
	}

	if err := s.store.VoidThrow(r.Context(), roundID, throwID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.publishRound(r, sessionID, roundID, "throw.voided")
	s.respondWithRound(w, r, roundID)
}

// publishRound fans out a round's current state under the given event name.
func (s *Server) publishRound(r *http.Request, sessionID, roundID int64, name string) {
	round, err := s.store.Round(r.Context(), roundID)
	if err != nil {
		s.log.Error("publishing round event", "event", name, "round", roundID, "error", err)
		return
	}
	s.hub.Publish(sessionID, name, map[string]any{"round": roundResponse(round)})
}

func (s *Server) respondWithRound(w http.ResponseWriter, r *http.Request, roundID int64) {
	round, err := s.store.Round(r.Context(), roundID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"round": roundResponse(round)})
}
