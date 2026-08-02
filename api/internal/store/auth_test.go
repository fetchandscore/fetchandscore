package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func hash(s string) []byte {
	return []byte("hash:" + s)
}

func testUser(t *testing.T, s *Store) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), "brad@example.com", "Brad")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// A sign-in link must work exactly once. If a second use succeeded, a link
// sitting in an inbox would stay a permanent key to the account.
func TestConsumeLoginToken_IsSingleUse(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	user := testUser(t, s)
	now := time.Now()

	if err := s.CreateLoginToken(ctx, user.ID, hash("tok"), now.Add(15*time.Minute)); err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	got, err := s.ConsumeLoginToken(ctx, hash("tok"), now)
	if err != nil {
		t.Fatalf("first ConsumeLoginToken: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("consumed token belongs to user %d, want %d", got.ID, user.ID)
	}

	if _, err := s.ConsumeLoginToken(ctx, hash("tok"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second ConsumeLoginToken returned %v, want ErrNotFound", err)
	}
}

func TestConsumeLoginToken_RejectsExpired(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	user := testUser(t, s)
	now := time.Now()

	if err := s.CreateLoginToken(ctx, user.ID, hash("old"), now.Add(-time.Minute)); err != nil {
		t.Fatalf("CreateLoginToken: %v", err)
	}

	if _, err := s.ConsumeLoginToken(ctx, hash("old"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsumeLoginToken on an expired token returned %v, want ErrNotFound", err)
	}
}

func TestConsumeLoginToken_RejectsUnknown(t *testing.T) {
	s := openTemp(t)

	if _, err := s.ConsumeLoginToken(context.Background(), hash("never-issued"), time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ConsumeLoginToken returned %v, want ErrNotFound", err)
	}
}

func TestAuthSession_LookupAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	user := testUser(t, s)
	now := time.Now()

	if err := s.CreateAuthSession(ctx, user.ID, hash("sess"), "test-agent", now.Add(24*time.Hour)); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}

	got, err := s.AuthSessionUser(ctx, hash("sess"), now)
	if err != nil {
		t.Fatalf("AuthSessionUser: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("session belongs to user %d, want %d", got.ID, user.ID)
	}

	if err := s.DeleteAuthSession(ctx, hash("sess")); err != nil {
		t.Fatalf("DeleteAuthSession: %v", err)
	}
	if _, err := s.AuthSessionUser(ctx, hash("sess"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AuthSessionUser after logout returned %v, want ErrNotFound", err)
	}
}

func TestAuthSessionUser_RejectsExpired(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	user := testUser(t, s)
	now := time.Now()

	if err := s.CreateAuthSession(ctx, user.ID, hash("stale"), "", now.Add(-time.Second)); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}

	if _, err := s.AuthSessionUser(ctx, hash("stale"), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AuthSessionUser on an expired session returned %v, want ErrNotFound", err)
	}
}

// Membership is by invitation, so the sign-in endpoint asks this question
// before it will send anything.
func TestPendingInvite(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	club, err := s.CreateClub(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	now := time.Now()

	if _, err := s.PendingInvite(ctx, "nobody@example.com", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingInvite for a stranger returned %v, want ErrNotFound", err)
	}

	invite, err := s.CreateInvite(ctx, NewInvite{
		ClubID:    club.ID,
		Email:     "Newcomer@Example.com",
		Role:      RoleMember,
		TokenHash: hash("inv"),
		ExpiresAt: now.Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// Addresses collate NOCASE, so the case they typed at sign-in is irrelevant.
	found, err := s.PendingInvite(ctx, "newcomer@example.com", now)
	if err != nil {
		t.Fatalf("PendingInvite: %v", err)
	}
	if found.ID != invite.ID || found.Role != RoleMember || found.ClubID != club.ID {
		t.Errorf("found invite %+v, want the created one", found)
	}

	if err := s.AcceptInvite(ctx, invite.ID, now); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if _, err := s.PendingInvite(ctx, "newcomer@example.com", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingInvite after acceptance returned %v, want ErrNotFound", err)
	}
}

func TestPendingInvite_RejectsExpired(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	club, err := s.CreateClub(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	now := time.Now()

	if _, err := s.CreateInvite(ctx, NewInvite{
		ClubID: club.ID, Email: "late@example.com", Role: RoleMember,
		TokenHash: hash("expired"), ExpiresAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	if _, err := s.PendingInvite(ctx, "late@example.com", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PendingInvite on an expired invite returned %v, want ErrNotFound", err)
	}
}
