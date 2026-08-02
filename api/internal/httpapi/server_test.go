package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/mail"
	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

const testOrigin = "https://fetchandscore.com"

type testServer struct {
	*httptest.Server
	store  *store.Store
	mailer *mail.Recorder
	club   store.Club
	round  store.Round
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	rec := &mail.Recorder{}
	authSvc := auth.New(st, rec, testOrigin)
	srv := NewServer(st, authSvc, NewHub(), Config{AllowedOrigin: testOrigin}, discardLogger())

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	club, err := st.CreateClub(ctx, "club", "Test Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}

	return &testServer{Server: ts, store: st, mailer: rec, club: club}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedRound builds a team, season and round so scoring endpoints have a target.
func (ts *testServer) seedRound(t *testing.T, handlerUserID int64) store.Round {
	t.Helper()
	ctx := context.Background()

	dog, err := ts.store.CreateDog(ctx, handlerUserID, "Sadie", nil, false)
	if err != nil {
		t.Fatalf("CreateDog: %v", err)
	}
	team, err := ts.store.CreateTeam(ctx, ts.club.ID, handlerUserID, dog.ID, "Sadie · Brad")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	season, err := ts.store.CreateSeason(ctx, store.NewSeason{
		ClubID: ts.club.ID, Name: "Season", Year: 2026,
		FormatKey: "60_all", Format: scoring.Format60All(),
		WeekCount: 5, StartsOn: "2026-08-01",
	})
	if err != nil {
		t.Fatalf("CreateSeason: %v", err)
	}
	entry, err := ts.store.CreateSeasonEntry(ctx, store.NewSeasonEntry{
		SeasonID: season.ID, TeamID: team.ID, Division: scoring.DivisionExpert,
	})
	if err != nil {
		t.Fatalf("CreateSeasonEntry: %v", err)
	}
	sess, err := ts.store.CreatePlaySession(ctx, store.NewPlaySession{
		ClubID: ts.club.ID, Name: "Week 1", StartsAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}
	if err := ts.store.AddSessionTeam(ctx, sess.ID, entry.ID, 1); err != nil {
		t.Fatalf("AddSessionTeam: %v", err)
	}
	round, err := ts.store.CreateRound(ctx, sess.ID, entry.ID, 1)
	if err != nil {
		t.Fatalf("CreateRound: %v", err)
	}
	ts.round = round
	return round
}

// client is a browser-like caller: it keeps cookies and sends an Origin.
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func (ts *testServer) client(t *testing.T) *client {
	t.Helper()
	jar := &cookieJar{cookies: map[string]*http.Cookie{}}
	return &client{t: t, base: ts.URL, http: &http.Client{Jar: nil, Transport: jar.transport()}}
}

// cookieJar is a minimal cookie store; net/http/cookiejar refuses to store
// cookies for the bare IP that httptest serves on.
type cookieJar struct {
	cookies map[string]*http.Cookie
}

func (j *cookieJar) transport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, c := range j.cookies {
			r.AddCookie(c)
		}
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		for _, c := range resp.Cookies() {
			if c.MaxAge < 0 {
				delete(j.cookies, c.Name)
				continue
			}
			j.cookies[c.Name] = c
		}
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (c *client) do(method, path string, body any, origin string) response {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshalling body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	out := response{status: resp.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (c *client) get(path string) response { return c.do(http.MethodGet, path, nil, testOrigin) }
func (c *client) post(path string, body any) response {
	return c.do(http.MethodPost, path, body, testOrigin)
}

// signIn runs the full invite-and-verify flow and leaves the client signed in.
func (ts *testServer) signIn(t *testing.T, c *client, email string, role store.Role) int64 {
	t.Helper()
	ctx := context.Background()

	authSvc := auth.New(ts.store, ts.mailer, testOrigin)
	if _, err := authSvc.Invite(ctx, ts.club.ID, email, role, nil); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	if got := c.post("/api/auth/request", map[string]string{"email": email}); got.status != http.StatusAccepted {
		t.Fatalf("auth request returned %d: %s", got.status, got.raw)
	}

	msg, ok := ts.mailer.Last()
	if !ok {
		t.Fatal("no sign-in email was sent")
	}
	token := msg.Text[strings.Index(msg.Text, "token=")+len("token="):]
	if i := strings.IndexAny(token, "\n \t"); i >= 0 {
		token = token[:i]
	}

	got := c.post("/api/auth/verify", map[string]string{"token": token})
	if got.status != http.StatusOK {
		t.Fatalf("verify returned %d: %s", got.status, got.raw)
	}

	user, err := ts.store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	return user.ID
}

// --- tests -----------------------------------------------------------------

func TestHealthAndReady(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	if got := c.get("/healthz"); got.status != http.StatusOK {
		t.Errorf("/healthz returned %d, want 200", got.status)
	}
	if got := c.get("/readyz"); got.status != http.StatusOK {
		t.Errorf("/readyz returned %d, want 200", got.status)
	}
}

func TestProtectedRoutesRejectAnonymousCallers(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	for _, path := range []string{"/api/me", "/api/dashboard", "/api/sessions/1", "/api/sessions/1/stream"} {
		if got := c.get(path); got.status != http.StatusUnauthorized {
			t.Errorf("GET %s returned %d, want 401", path, got.status)
		}
	}
}

// The sign-in endpoint must not reveal whether an address is known.
func TestAuthRequest_LooksIdenticalForKnownAndUnknownAddresses(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	stranger := c.post("/api/auth/request", map[string]string{"email": "stranger@example.com"})
	if stranger.status != http.StatusAccepted {
		t.Fatalf("unknown address returned %d, want 202", stranger.status)
	}
	if sent := ts.mailer.Sent(); len(sent) != 0 {
		t.Errorf("sent %d messages to an uninvited address, want 0", len(sent))
	}

	authSvc := auth.New(ts.store, ts.mailer, testOrigin)
	if _, err := authSvc.Invite(context.Background(), ts.club.ID, "member@example.com", store.RoleMember, nil); err != nil {
		t.Fatalf("Invite: %v", err)
	}

	invited := c.post("/api/auth/request", map[string]string{"email": "member@example.com"})
	if invited.status != stranger.status {
		t.Errorf("invited address returned %d but stranger returned %d; they must be indistinguishable",
			invited.status, stranger.status)
	}
}

func TestAuthRequest_IsRateLimited(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	var limited bool
	for range linkRequestLimit + 2 {
		if got := c.post("/api/auth/request", map[string]string{"email": "spam@example.com"}); got.status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("the sign-in endpoint never rate limited a burst of requests")
	}
}

// The whole point: sign in, start a round, score it, confirm it.
func TestScoringFlow(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	userID := ts.signIn(t, c, "brad@example.com", store.RoleScorekeeper)
	round := ts.seedRound(t, userID)
	path := func(suffix string) string {
		return "/api/rounds/" + itoa(round.ID) + suffix
	}

	if got := c.post(path("/start"), nil); got.status != http.StatusOK {
		t.Fatalf("start returned %d: %s", got.status, got.raw)
	}

	for _, th := range []map[string]any{
		{"client_id": "a", "zone": "40-50", "air": true},
		{"client_id": "b", "zone": "30-40", "air": false},
		{"client_id": "c", "zone": "miss", "air": false},
	} {
		if got := c.post(path("/throws"), th); got.status != http.StatusOK {
			t.Fatalf("throw %v returned %d: %s", th["client_id"], got.status, got.raw)
		}
	}

	// A retry of an already-recorded throw must not score twice.
	if got := c.post(path("/throws"), map[string]any{"client_id": "a", "zone": "40-50", "air": true}); got.status != http.StatusOK {
		t.Fatalf("retried throw returned %d: %s", got.status, got.raw)
	}

	got := c.post(path("/confirm"), nil)
	if got.status != http.StatusOK {
		t.Fatalf("confirm returned %d: %s", got.status, got.raw)
	}

	roundBody, _ := got.body["round"].(map[string]any)
	if total, _ := roundBody["total_points"].(float64); total != 8.5 {
		t.Errorf("total_points = %v, want 8.5 (5.5 air catch + 3)", roundBody["total_points"])
	}
	if roundBody["status"] != "confirmed" {
		t.Errorf("status = %v, want confirmed", roundBody["status"])
	}

	// A confirmed round is final.
	if late := c.post(path("/throws"), map[string]any{"client_id": "late", "zone": "40-50"}); late.status != http.StatusConflict {
		t.Errorf("throw after confirm returned %d, want 409", late.status)
	}
}

// Without a client id the write would not be idempotent, so it is refused
// rather than silently accepted.
func TestAddThrow_RequiresAClientID(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	userID := ts.signIn(t, c, "brad@example.com", store.RoleScorekeeper)
	round := ts.seedRound(t, userID)

	got := c.post("/api/rounds/"+itoa(round.ID)+"/throws", map[string]any{"zone": "40-50"})
	if got.status != http.StatusBadRequest {
		t.Errorf("throw without client_id returned %d, want 400", got.status)
	}
}

func TestAddThrow_RejectsAnUnknownZone(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	userID := ts.signIn(t, c, "brad@example.com", store.RoleScorekeeper)
	round := ts.seedRound(t, userID)

	got := c.post("/api/rounds/"+itoa(round.ID)+"/throws",
		map[string]any{"client_id": "x", "zone": "60-70"})
	if got.status != http.StatusBadRequest {
		t.Errorf("throw with an unknown zone returned %d, want 400", got.status)
	}
}

// A plain member may watch but not score.
func TestScoring_RequiresScorekeeperRole(t *testing.T) {
	ts := newTestServer(t)

	owner := ts.client(t)
	ownerID := ts.signIn(t, owner, "brad@example.com", store.RoleCaptain)
	round := ts.seedRound(t, ownerID)

	spectator := ts.client(t)
	ts.signIn(t, spectator, "watcher@example.com", store.RoleMember)

	if got := spectator.post("/api/rounds/"+itoa(round.ID)+"/start", nil); got.status != http.StatusForbidden {
		t.Errorf("member starting a round returned %d, want 403", got.status)
	}
	if got := spectator.get("/api/sessions/" + itoa(round.PlaySessionID)); got.status != http.StatusOK {
		t.Errorf("member reading a session returned %d, want 200", got.status)
	}
}

// Someone outside the club gets 404, not 403: whether the club exists is not
// theirs to learn.
func TestSession_HiddenFromNonMembers(t *testing.T) {
	ts := newTestServer(t)

	owner := ts.client(t)
	ownerID := ts.signIn(t, owner, "brad@example.com", store.RoleCaptain)
	round := ts.seedRound(t, ownerID)

	other, err := ts.store.CreateClub(context.Background(), "other", "Other")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	outsider := ts.client(t)
	authSvc := auth.New(ts.store, ts.mailer, testOrigin)
	if _, err := authSvc.Invite(context.Background(), other.ID, "outsider@example.com", store.RoleCaptain, nil); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if got := outsider.post("/api/auth/request", map[string]string{"email": "outsider@example.com"}); got.status != http.StatusAccepted {
		t.Fatalf("auth request: %d", got.status)
	}
	msg, _ := ts.mailer.Last()
	token := msg.Text[strings.Index(msg.Text, "token=")+len("token="):]
	if i := strings.IndexAny(token, "\n \t"); i >= 0 {
		token = token[:i]
	}
	if got := outsider.post("/api/auth/verify", map[string]string{"token": token}); got.status != http.StatusOK {
		t.Fatalf("verify: %d", got.status)
	}

	if got := outsider.get("/api/sessions/" + itoa(round.PlaySessionID)); got.status != http.StatusNotFound {
		t.Errorf("outsider reading a session returned %d, want 404", got.status)
	}
}

// Confirmed results are public; rounds still in progress are not.
func TestPublicSession_ShowsOnlyConfirmedRounds(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	userID := ts.signIn(t, c, "brad@example.com", store.RoleScorekeeper)
	round := ts.seedRound(t, userID)

	anon := ts.client(t)
	path := "/api/public/sessions/" + itoa(round.PlaySessionID)

	got := anon.get(path)
	if got.status != http.StatusOK {
		t.Fatalf("public session returned %d, want 200 without a cookie", got.status)
	}
	if n := confirmedRoundCount(got.body); n != 0 {
		t.Errorf("an unconfirmed round was public; got %d rounds", n)
	}

	c.post("/api/rounds/"+itoa(round.ID)+"/start", nil)
	c.post("/api/rounds/"+itoa(round.ID)+"/throws", map[string]any{"client_id": "a", "zone": "40-50"})
	c.post("/api/rounds/"+itoa(round.ID)+"/confirm", nil)

	if n := confirmedRoundCount(anon.get(path).body); n != 1 {
		t.Errorf("confirmed round count = %d, want 1", n)
	}
}

func confirmedRoundCount(body map[string]any) int {
	teams, _ := body["teams"].([]any)
	total := 0
	for _, raw := range teams {
		team, _ := raw.(map[string]any)
		rounds, _ := team["rounds"].([]any)
		total += len(rounds)
	}
	return total
}

// The API is called cross-origin from GitHub Pages, so the origin check is
// load-bearing rather than decorative.
func TestCORS(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)

	allowed := c.do(http.MethodPost, "/api/auth/request",
		map[string]string{"email": "a@b.c"}, testOrigin)
	if allowed.status != http.StatusAccepted {
		t.Errorf("allowed origin returned %d, want 202", allowed.status)
	}

	blocked := c.do(http.MethodPost, "/api/auth/request",
		map[string]string{"email": "a@b.c"}, "https://evil.example.com")
	if blocked.status != http.StatusForbidden {
		t.Errorf("foreign origin returned %d, want 403", blocked.status)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// The session cookie must be unreachable from script.
func TestSessionCookie_IsHttpOnly(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	ts.signIn(t, c, "brad@example.com", store.RoleMember)

	authSvc := auth.New(ts.store, ts.mailer, testOrigin)
	if _, err := authSvc.Invite(context.Background(), ts.club.ID, "second@example.com", store.RoleMember, nil); err != nil {
		t.Fatalf("Invite: %v", err)
	}
	fresh := ts.client(t)
	fresh.post("/api/auth/request", map[string]string{"email": "second@example.com"})
	msg, _ := ts.mailer.Last()
	token := msg.Text[strings.Index(msg.Text, "token=")+len("token="):]
	if i := strings.IndexAny(token, "\n \t"); i >= 0 {
		token = token[:i]
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/verify",
		strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var found bool
	for _, cookie := range resp.Cookies() {
		if cookie.Name != sessionCookie {
			continue
		}
		found = true
		if !cookie.HttpOnly {
			t.Error("session cookie is not HttpOnly")
		}
		if cookie.SameSite != http.SameSiteLaxMode {
			t.Errorf("session cookie SameSite = %v, want Lax", cookie.SameSite)
		}
	}
	if !found {
		t.Fatal("verify did not set a session cookie")
	}
}

func TestLogout_ClearsTheSession(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	ts.signIn(t, c, "brad@example.com", store.RoleMember)

	if got := c.get("/api/me"); got.status != http.StatusOK {
		t.Fatalf("/api/me before logout returned %d", got.status)
	}
	if got := c.post("/api/auth/logout", nil); got.status != http.StatusOK {
		t.Fatalf("logout returned %d", got.status)
	}
	if got := c.get("/api/me"); got.status != http.StatusUnauthorized {
		t.Errorf("/api/me after logout returned %d, want 401", got.status)
	}
}

func TestCreateInvite_RequiresCaptain(t *testing.T) {
	ts := newTestServer(t)

	member := ts.client(t)
	ts.signIn(t, member, "member@example.com", store.RoleScorekeeper)
	got := member.post("/api/clubs/"+itoa(ts.club.ID)+"/invites",
		map[string]string{"email": "new@example.com", "role": "member"})
	if got.status != http.StatusForbidden {
		t.Errorf("scorekeeper inviting returned %d, want 403", got.status)
	}

	captain := ts.client(t)
	ts.signIn(t, captain, "captain@example.com", store.RoleCaptain)
	got = captain.post("/api/clubs/"+itoa(ts.club.ID)+"/invites",
		map[string]string{"email": "new@example.com", "role": "member"})
	if got.status != http.StatusCreated {
		t.Fatalf("captain inviting returned %d: %s", got.status, got.raw)
	}
	if link, _ := got.body["invite_url"].(string); !strings.Contains(link, "invite=") {
		t.Errorf("invite_url = %q, want a link carrying a token", link)
	}
}

func TestCreateInvite_RejectsAnUnknownRole(t *testing.T) {
	ts := newTestServer(t)
	captain := ts.client(t)
	ts.signIn(t, captain, "captain@example.com", store.RoleCaptain)

	got := captain.post("/api/clubs/"+itoa(ts.club.ID)+"/invites",
		map[string]string{"email": "new@example.com", "role": "supreme-leader"})
	if got.status != http.StatusBadRequest {
		t.Errorf("unknown role returned %d, want 400", got.status)
	}
}

func TestDashboard_ReturnsArraysNotNulls(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	ts.signIn(t, c, "brad@example.com", store.RoleMember)

	got := c.get("/api/dashboard")
	if got.status != http.StatusOK {
		t.Fatalf("dashboard returned %d: %s", got.status, got.raw)
	}
	for _, key := range []string{"active", "upcoming", "past"} {
		if _, ok := got.body[key].([]any); !ok {
			t.Errorf("%s = %v, want an array so the client can iterate it", key, got.body[key])
		}
	}
}

// The stream is the live feed; a spectator's browser opens it and must get
// event-stream framing plus an id it can resume from.
func TestStream_SendsEventsToWatchers(t *testing.T) {
	ts := newTestServer(t)
	c := ts.client(t)
	userID := ts.signIn(t, c, "brad@example.com", store.RoleScorekeeper)
	round := ts.seedRound(t, userID)

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/sessions/"+itoa(round.PlaySessionID)+"/stream", nil)
	req.Header.Set("Origin", testOrigin)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("opening stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc strings.Builder
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), "event: throw.added") {
					received <- acc.String()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Give the subscription a moment to register before publishing.
	time.Sleep(100 * time.Millisecond)
	c.post("/api/rounds/"+itoa(round.ID)+"/throws", map[string]any{"client_id": "a", "zone": "40-50"})

	select {
	case body := <-received:
		if !strings.Contains(body, "id: ") {
			t.Error("stream event carried no id, so a client could not resume")
		}
		if !strings.Contains(body, "total_points") {
			t.Error("stream event carried no round total")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a throw.added event")
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
