// Package auth handles who someone is: invitations, sign-in links, and
// sessions.
//
// There are no passwords. A club captain invites an address; that address can
// then ask for a one-time link, and following it establishes a session. Tokens
// are random, single-use, short-lived, and stored only as hashes.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/mail"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

// ErrInvalidToken covers every reason a token will not be honoured: unknown,
// already used, expired, or malformed. They are deliberately indistinguishable
// to the caller, so a probe learns nothing from the difference.
var ErrInvalidToken = errors.New("invalid or expired token")

const (
	// LinkLifetime is how long a sign-in link stays good. Long enough to walk
	// to your inbox, short enough that a forwarded email is not a key.
	LinkLifetime = 15 * time.Minute

	// SessionLifetime is how long a signed-in session lasts. A league season is
	// five weeks, and being asked to sign in again mid-season is a papercut.
	SessionLifetime = 60 * 24 * time.Hour

	// InviteLifetime is how long an invitation stays open.
	InviteLifetime = 14 * 24 * time.Hour

	// tokenBytes is the entropy in each token. 32 bytes is 256 bits, which is
	// not guessable by any means available to anyone.
	tokenBytes = 32
)

// Service issues and validates credentials.
type Service struct {
	store  *store.Store
	mailer mail.Sender
	// baseURL is the public origin of the web app, used to build sign-in links.
	baseURL string
	// now is injectable so tests can move time without sleeping.
	now func() time.Time
}

func New(st *store.Store, mailer mail.Sender, baseURL string) *Service {
	return &Service{
		store:   st,
		mailer:  mailer,
		baseURL: strings.TrimSuffix(baseURL, "/"),
		now:     time.Now,
	}
}

// newToken returns a fresh token and the hash to store against it.
func newToken() (token string, hash []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generating token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, hashToken(token), nil
}

// hashToken is what the database stores. Tokens themselves never persist, so a
// copy of the file cannot be replayed.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Invite opens membership of a club to an email address and returns the
// invitation link for the captain to pass on.
func (s *Service) Invite(ctx context.Context, clubID int64, email string, role store.Role, invitedBy *int64) (string, error) {
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}

	if _, err := s.store.CreateInvite(ctx, store.NewInvite{
		ClubID:    clubID,
		Email:     normalizeEmail(email),
		Role:      role,
		TokenHash: hash,
		InvitedBy: invitedBy,
		ExpiresAt: s.now().Add(InviteLifetime),
	}); err != nil {
		return "", err
	}

	return s.baseURL + "/auth.html?invite=" + url.QueryEscape(token), nil
}

// RequestLink emails a sign-in link, if and only if the address is entitled to
// one: either an existing user, or an address with a pending invitation.
//
// It reports success either way. Telling a caller that an address is unknown
// would turn this endpoint into a membership oracle.
func (s *Service) RequestLink(ctx context.Context, email string) error {
	email = normalizeEmail(email)
	now := s.now()

	user, err := s.store.UserByEmail(ctx, email)
	switch {
	case err == nil:
		// An existing member. Fall through and send.
	case errors.Is(err, store.ErrNotFound):
		// Only an outstanding invitation earns a first sign-in.
		if _, inviteErr := s.store.PendingInvite(ctx, email, now); inviteErr != nil {
			if errors.Is(inviteErr, store.ErrNotFound) {
				return nil
			}
			return inviteErr
		}
		user, err = s.store.CreateUser(ctx, email, "")
		if err != nil {
			return err
		}
	default:
		return err
	}

	token, hash, err := newToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateLoginToken(ctx, user.ID, hash, now.Add(LinkLifetime)); err != nil {
		return err
	}

	return s.mailer.Send(ctx, s.signInMessage(email, token))
}

// signInMessage builds the sign-in email.
//
// The link is a GET to a page that then POSTs the token back. Mail security
// scanners routinely fetch links to check them, and a token consumed by a GET
// would be burned before the recipient ever clicked it.
func (s *Service) signInMessage(email, token string) mail.Message {
	link := s.baseURL + "/auth.html?token=" + url.QueryEscape(token)

	return mail.Message{
		To:      email,
		Subject: "Your Fetch and Score sign-in link",
		Text: "Open this link to sign in to Fetch and Score:\n\n" +
			link + "\n\n" +
			"The link works once and expires in 15 minutes.\n" +
			"If you did not ask to sign in, you can ignore this email.\n",
		HTML: `<p>Open this link to sign in to Fetch and Score:</p>` +
			`<p><a href="` + link + `">Sign in</a></p>` +
			`<p>The link works once and expires in 15 minutes.<br>` +
			`If you did not ask to sign in, you can ignore this email.</p>`,
	}
}

// Verify redeems a sign-in token and returns a new session token.
//
// If the user was invited, this is where the invitation is spent and the club
// membership is created.
func (s *Service) Verify(ctx context.Context, token, userAgent string) (string, store.User, error) {
	if token == "" {
		return "", store.User{}, ErrInvalidToken
	}
	now := s.now()

	user, err := s.store.ConsumeLoginToken(ctx, hashToken(token), now)
	if errors.Is(err, store.ErrNotFound) {
		return "", store.User{}, ErrInvalidToken
	}
	if err != nil {
		return "", store.User{}, err
	}

	if err := s.acceptPendingInvite(ctx, user, now); err != nil {
		return "", store.User{}, err
	}

	sessionToken, sessionHash, err := newToken()
	if err != nil {
		return "", store.User{}, err
	}
	if err := s.store.CreateAuthSession(ctx, user.ID, sessionHash, userAgent, now.Add(SessionLifetime)); err != nil {
		return "", store.User{}, err
	}
	return sessionToken, user, nil
}

// acceptPendingInvite joins a user to the club that invited them, if one did.
// A user signing in again simply has no pending invitation.
func (s *Service) acceptPendingInvite(ctx context.Context, user store.User, now time.Time) error {
	invite, err := s.store.PendingInvite(ctx, user.Email, now)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := s.store.AddClubMember(ctx, invite.ClubID, user.ID, invite.Role); err != nil {
		return err
	}
	return s.store.AcceptInvite(ctx, invite.ID, now)
}

// Authenticate resolves a session token to its user.
func (s *Service) Authenticate(ctx context.Context, sessionToken string) (store.User, error) {
	if sessionToken == "" {
		return store.User{}, ErrInvalidToken
	}
	user, err := s.store.AuthSessionUser(ctx, hashToken(sessionToken), s.now())
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrInvalidToken
	}
	if err != nil {
		return store.User{}, err
	}
	return user, nil
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	if sessionToken == "" {
		return nil
	}
	return s.store.DeleteAuthSession(ctx, hashToken(sessionToken))
}

// normalizeEmail trims and lowercases an address. The column collates NOCASE
// as well, but normalizing on the way in keeps stored data tidy.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
