package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/mail"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

type harness struct {
	svc    *Service
	st     *store.Store
	mailer *mail.Recorder
	club   store.Club
}

func newHarness(t *testing.T) harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	club, err := st.CreateClub(context.Background(), "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}

	rec := &mail.Recorder{}
	return harness{
		svc:    New(st, rec, "https://fetchandscore.com"),
		st:     st,
		mailer: rec,
		club:   club,
	}
}

func (h harness) invite(t *testing.T, email string, role store.Role) {
	t.Helper()
	if _, err := h.svc.Invite(context.Background(), h.club.ID, email, role, nil); err != nil {
		t.Fatalf("Invite(%s): %v", email, err)
	}
}

// tokenFromLastEmail pulls the token out of the link that was sent.
func (h harness) tokenFromLastEmail(t *testing.T) string {
	t.Helper()
	msg, ok := h.mailer.Last()
	if !ok {
		t.Fatal("no email was sent")
	}
	const marker = "token="
	i := strings.Index(msg.Text, marker)
	if i < 0 {
		t.Fatalf("no token in the email body:\n%s", msg.Text)
	}
	tok := msg.Text[i+len(marker):]
	if j := strings.IndexAny(tok, "\n \t"); j >= 0 {
		tok = tok[:j]
	}
	return tok
}

// An address nobody invited gets no mail. The endpoint still reports success,
// so it cannot be used to discover who has an account.
func TestRequestLink_SilentlyIgnoresUninvitedAddresses(t *testing.T) {
	h := newHarness(t)

	if err := h.svc.RequestLink(context.Background(), "stranger@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}

	if sent := h.mailer.Sent(); len(sent) != 0 {
		t.Errorf("sent %d messages to an uninvited address, want 0", len(sent))
	}
}

func TestRequestLink_SendsALinkToAnInvitedAddress(t *testing.T) {
	h := newHarness(t)
	h.invite(t, "newcomer@example.com", store.RoleMember)

	if err := h.svc.RequestLink(context.Background(), "newcomer@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}

	msg, ok := h.mailer.Last()
	if !ok {
		t.Fatal("no email was sent to an invited address")
	}
	if msg.To != "newcomer@example.com" {
		t.Errorf("sent to %q, want the invited address", msg.To)
	}
	if !strings.Contains(msg.Text, "https://fetchandscore.com") {
		t.Errorf("link does not point at the site:\n%s", msg.Text)
	}
}

// The whole point of the flow: the token in the mail becomes a session.
func TestVerify_EstablishesASessionAndClubMembership(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.invite(t, "newcomer@example.com", store.RoleScorekeeper)

	if err := h.svc.RequestLink(ctx, "newcomer@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}

	sessionToken, user, err := h.svc.Verify(ctx, h.tokenFromLastEmail(t), "test-agent")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if sessionToken == "" {
		t.Fatal("Verify returned an empty session token")
	}
	if user.Email != "newcomer@example.com" {
		t.Errorf("session belongs to %q, want the invited address", user.Email)
	}

	role, err := h.st.MemberRole(ctx, h.club.ID, user.ID)
	if err != nil {
		t.Fatalf("MemberRole: %v", err)
	}
	if role != store.RoleScorekeeper {
		t.Errorf("joined as %q, want the invited role %q", role, store.RoleScorekeeper)
	}

	got, err := h.svc.Authenticate(ctx, sessionToken)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("Authenticate resolved user %d, want %d", got.ID, user.ID)
	}
}

// Mail scanners follow links. A token that survives being fetched but is
// consumed only on an explicit verify is the difference between a working
// sign-in and a mystifying one.
func TestVerify_TokenIsSingleUse(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.invite(t, "newcomer@example.com", store.RoleMember)

	if err := h.svc.RequestLink(ctx, "newcomer@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}
	token := h.tokenFromLastEmail(t)

	if _, _, err := h.svc.Verify(ctx, token, ""); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if _, _, err := h.svc.Verify(ctx, token, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second Verify returned %v, want ErrInvalidToken", err)
	}
}

func TestVerify_RejectsGarbage(t *testing.T) {
	h := newHarness(t)

	for _, token := range []string{"", "not-a-token", strings.Repeat("a", 64)} {
		if _, _, err := h.svc.Verify(context.Background(), token, ""); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%q) returned %v, want ErrInvalidToken", token, err)
		}
	}
}

// Signing in a second time must reuse the account rather than colliding on the
// unique email.
func TestRequestLink_ReturningUserKeepsTheirAccount(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.invite(t, "brad@example.com", store.RoleCaptain)

	if err := h.svc.RequestLink(ctx, "brad@example.com"); err != nil {
		t.Fatalf("first RequestLink: %v", err)
	}
	_, first, err := h.svc.Verify(ctx, h.tokenFromLastEmail(t), "")
	if err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// The invite is spent, but an existing member can still sign in.
	if err := h.svc.RequestLink(ctx, "brad@example.com"); err != nil {
		t.Fatalf("second RequestLink: %v", err)
	}
	_, second, err := h.svc.Verify(ctx, h.tokenFromLastEmail(t), "")
	if err != nil {
		t.Fatalf("second Verify: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("second sign-in created user %d, want the existing %d", second.ID, first.ID)
	}
}

func TestLogout_InvalidatesTheSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.invite(t, "brad@example.com", store.RoleMember)

	if err := h.svc.RequestLink(ctx, "brad@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}
	token, _, err := h.svc.Verify(ctx, h.tokenFromLastEmail(t), "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if err := h.svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := h.svc.Authenticate(ctx, token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Authenticate after logout returned %v, want ErrInvalidToken", err)
	}
}

// A sign-in link is a bearer credential sitting in an inbox; it must go stale.
func TestRequestLink_TokenExpires(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.invite(t, "brad@example.com", store.RoleMember)

	if err := h.svc.RequestLink(ctx, "brad@example.com"); err != nil {
		t.Fatalf("RequestLink: %v", err)
	}
	token := h.tokenFromLastEmail(t)

	h.svc.now = func() time.Time { return time.Now().Add(LinkLifetime + time.Minute) }

	if _, _, err := h.svc.Verify(ctx, token, ""); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify on an expired link returned %v, want ErrInvalidToken", err)
	}
}
