package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tokens are never stored. Only their hashes reach the database, so a leaked
// copy of the file cannot be replayed into a session or a sign-in.

// Invite is a standing permission for one address to join one club.
type Invite struct {
	ID      int64
	ClubID  int64
	Email   string
	Role    Role
	Expires time.Time
}

// NewInvite describes an invitation to create.
type NewInvite struct {
	ClubID    int64
	Email     string
	Role      Role
	TokenHash []byte
	InvitedBy *int64
	ExpiresAt time.Time
}

func (s *Store) CreateInvite(ctx context.Context, in NewInvite) (Invite, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO invites (club_id, email, role, token_hash, invited_by, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.ClubID, in.Email, string(in.Role), in.TokenHash, in.InvitedBy,
		millis(time.Now()), millis(in.ExpiresAt))
	if err != nil {
		return Invite{}, fmt.Errorf("inviting %s: %w", in.Email, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Invite{}, err
	}
	return Invite{
		ID: id, ClubID: in.ClubID, Email: in.Email,
		Role: in.Role, Expires: in.ExpiresAt,
	}, nil
}

// PendingInvite finds an unaccepted, unexpired invitation for an address.
//
// The sign-in endpoint asks this before sending anything, which is what keeps
// the mail path from becoming an open relay.
func (s *Store) PendingInvite(ctx context.Context, email string, now time.Time) (Invite, error) {
	var (
		inv     Invite
		role    string
		expires int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, club_id, email, role, expires_at
		   FROM invites
		  WHERE email = ? AND accepted_at IS NULL AND expires_at > ?
		  ORDER BY created_at DESC
		  LIMIT 1`,
		email, millis(now),
	).Scan(&inv.ID, &inv.ClubID, &inv.Email, &role, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Invite{}, ErrNotFound
	}
	if err != nil {
		return Invite{}, fmt.Errorf("looking up invite for %s: %w", email, err)
	}
	inv.Role = Role(role)
	inv.Expires = fromMillis(expires)
	return inv, nil
}

func (s *Store) AcceptInvite(ctx context.Context, inviteID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE invites SET accepted_at = ? WHERE id = ? AND accepted_at IS NULL`,
		millis(at), inviteID)
	if err != nil {
		return fmt.Errorf("accepting invite %d: %w", inviteID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("invite %d: %w", inviteID, ErrNotFound)
	}
	return nil
}

func (s *Store) CreateLoginToken(ctx context.Context, userID int64, tokenHash []byte, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO login_tokens (user_id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		userID, tokenHash, millis(time.Now()), millis(expiresAt))
	if err != nil {
		return fmt.Errorf("issuing login token: %w", err)
	}
	return nil
}

// ConsumeLoginToken redeems a sign-in token and returns its owner.
//
// The update is the guard: marking the token used in the same statement that
// selects it means two simultaneous redemptions cannot both succeed.
func (s *Store) ConsumeLoginToken(ctx context.Context, tokenHash []byte, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE login_tokens
		    SET used_at = ?
		  WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		millis(now), tokenHash, millis(now))
	if err != nil {
		return User{}, fmt.Errorf("consuming login token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return User{}, err
	}
	if n == 0 {
		return User{}, ErrNotFound
	}

	var userID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM login_tokens WHERE token_hash = ?`, tokenHash,
	).Scan(&userID); err != nil {
		return User{}, fmt.Errorf("reading consumed token: %w", err)
	}
	return s.UserByID(ctx, userID)
}

func (s *Store) CreateAuthSession(ctx context.Context, userID int64, tokenHash []byte, userAgent string, expiresAt time.Time) error {
	now := millis(time.Now())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_sessions (user_id, token_hash, user_agent, created_at, last_seen_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		userID, tokenHash, userAgent, now, now, millis(expiresAt))
	if err != nil {
		return fmt.Errorf("creating auth session: %w", err)
	}
	return nil
}

// AuthSessionUser resolves a session cookie to its user, refreshing the
// last-seen stamp so an active session can be told apart from an abandoned one.
func (s *Store) AuthSessionUser(ctx context.Context, tokenHash []byte, now time.Time) (User, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id FROM auth_sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, millis(now),
	).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("looking up auth session: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth_sessions SET last_seen_at = ? WHERE token_hash = ?`,
		millis(now), tokenHash); err != nil {
		return User{}, fmt.Errorf("touching auth session: %w", err)
	}
	return s.UserByID(ctx, userID)
}

func (s *Store) DeleteAuthSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("deleting auth session: %w", err)
	}
	return nil
}

// DeleteExpiredAuth clears spent tokens and dead sessions. Called on a timer by
// the server so the tables do not grow without bound.
func (s *Store) DeleteExpiredAuth(ctx context.Context, now time.Time) error {
	cutoff := millis(now)
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM login_tokens WHERE expires_at < ? OR used_at IS NOT NULL`, cutoff); err != nil {
		return fmt.Errorf("clearing login tokens: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE expires_at < ?`, cutoff); err != nil {
		return fmt.Errorf("clearing auth sessions: %w", err)
	}
	return nil
}
