package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// Every club-scoped permission check runs through Role.AtLeast, so its
// ordering is worth pinning down explicitly.
func TestRole_AtLeast(t *testing.T) {
	tests := []struct {
		have Role
		min  Role
		want bool
	}{
		{RoleAdmin, RoleCaptain, true},
		{RoleCaptain, RoleCaptain, true},
		{RoleScorekeeper, RoleCaptain, false},
		{RoleMember, RoleScorekeeper, false},
		{RoleScorekeeper, RoleMember, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleCaptain, RoleAdmin, false},
	}

	for _, tc := range tests {
		if got := tc.have.AtLeast(tc.min); got != tc.want {
			t.Errorf("Role(%q).AtLeast(%q) = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

// Addresses are collated NOCASE, because a handler typing their email with a
// capital should not create a second account.
func TestUserByEmail_IsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	created, err := s.CreateUser(ctx, "Brad@Example.com", "Brad")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	found, err := s.UserByEmail(ctx, "brad@example.COM")
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("found user %d, want %d", found.ID, created.ID)
	}
}

func TestUserByEmail_UnknownIsNotFound(t *testing.T) {
	s := openTemp(t)

	if _, err := s.UserByEmail(context.Background(), "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UserByEmail returned %v, want ErrNotFound", err)
	}
}

func TestUserByID_RoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	created, err := s.CreateUser(ctx, "brad@example.com", "Brad")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	found, err := s.UserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if found.Email != "brad@example.com" || found.Name != "Brad" {
		t.Errorf("UserByID returned %+v, want the created user", found)
	}

	if _, err := s.UserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("UserByID(9999) returned %v, want ErrNotFound", err)
	}
}

func TestMemberRole(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	user, err := s.CreateUser(ctx, "brad@example.com", "Brad")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	club, err := s.CreateClub(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}

	if _, err := s.MemberRole(ctx, club.ID, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MemberRole before joining returned %v, want ErrNotFound", err)
	}

	if err := s.AddClubMember(ctx, club.ID, user.ID, RoleScorekeeper); err != nil {
		t.Fatalf("AddClubMember: %v", err)
	}
	role, err := s.MemberRole(ctx, club.ID, user.ID)
	if err != nil {
		t.Fatalf("MemberRole: %v", err)
	}
	if role != RoleScorekeeper {
		t.Errorf("role = %q, want %q", role, RoleScorekeeper)
	}

	// Re-adding promotes rather than failing on the primary key.
	if err := s.AddClubMember(ctx, club.ID, user.ID, RoleCaptain); err != nil {
		t.Fatalf("promoting member: %v", err)
	}
	role, err = s.MemberRole(ctx, club.ID, user.ID)
	if err != nil {
		t.Fatalf("MemberRole after promotion: %v", err)
	}
	if role != RoleCaptain {
		t.Errorf("role after promotion = %q, want %q", role, RoleCaptain)
	}
}

// A season stores its format as values, not as a reference to a preset, so a
// custom format must survive the round trip intact.
func TestSeason_RoundTripsACustomFormat(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	club, err := s.CreateClub(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}

	custom := scoring.Format{
		Name:           "custom",
		RoundSeconds:   120,
		RoundsPerWeek:  2,
		ScoredThrowCap: 7,
		TinyWeeklyCap:  90,
		Handicaps: map[scoring.Division]scoring.HalfPoints{
			scoring.DivisionJunior:  25,
			scoring.DivisionHandler: 20,
			scoring.DivisionMaster:  15, // 7.5 points, which an integer column would lose
			scoring.DivisionExpert:  0,
		},
		CueSeconds: []int{30, 15, 5, 4, 3, 2, 1},
	}

	created, err := s.CreateSeason(ctx, NewSeason{
		ClubID: club.ID, Name: "Custom", Year: 2026,
		FormatKey: "custom", Format: custom, WeekCount: 4, StartsOn: "2026-09-01",
	})
	if err != nil {
		t.Fatalf("CreateSeason: %v", err)
	}

	got, err := s.Season(ctx, created.ID)
	if err != nil {
		t.Fatalf("Season: %v", err)
	}

	if got.Format.RoundSeconds != 120 {
		t.Errorf("RoundSeconds = %d, want 120", got.Format.RoundSeconds)
	}
	if got.Format.ScoredThrowCap != 7 {
		t.Errorf("ScoredThrowCap = %d, want 7", got.Format.ScoredThrowCap)
	}
	if got.Format.TinyWeeklyCap != 90 {
		t.Errorf("TinyWeeklyCap = %d, want 90", got.Format.TinyWeeklyCap)
	}
	if got.Format.Handicaps[scoring.DivisionMaster] != 15 {
		t.Errorf("master handicap = %v, want 15 half-points (7.5)", got.Format.Handicaps[scoring.DivisionMaster])
	}
	if len(got.Format.CueSeconds) != 7 || got.Format.CueSeconds[1] != 15 {
		t.Errorf("CueSeconds = %v, want the custom 30/15/5-1 marks", got.Format.CueSeconds)
	}
	if got.WeekCount != 4 {
		t.Errorf("WeekCount = %d, want 4", got.WeekCount)
	}

	if _, err := s.Season(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Season(9999) returned %v, want ErrNotFound", err)
	}
}

func TestPlaySessionByID(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	club, err := s.CreateClub(ctx, "club", "Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	start := time.Now().Truncate(time.Millisecond)
	created, err := s.CreatePlaySession(ctx, NewPlaySession{
		ClubID: club.ID, Name: "Week 1", StartsAt: start,
	})
	if err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}

	got, err := s.PlaySessionByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("PlaySessionByID: %v", err)
	}
	if got.Name != "Week 1" || got.Status != "scheduled" {
		t.Errorf("session = %+v, want name \"Week 1\" and status \"scheduled\"", got)
	}
	if !got.StartsAt.Equal(start.UTC()) {
		t.Errorf("StartsAt = %v, want %v", got.StartsAt, start.UTC())
	}

	if _, err := s.PlaySessionByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("PlaySessionByID(9999) returned %v, want ErrNotFound", err)
	}
}

// The session view lists rounds by running order, not by insertion order.
func TestSessionRounds_OrderedByRunningOrder(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	// A second team, added to the session ahead of the first.
	dog, err := f.store.CreateDog(ctx, f.user.ID, "Rex", nil, false)
	if err != nil {
		t.Fatalf("CreateDog: %v", err)
	}
	team, err := f.store.CreateTeam(ctx, f.club.ID, f.user.ID, dog.ID, "Rex · Brad")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	entry, err := f.store.CreateSeasonEntry(ctx, NewSeasonEntry{
		SeasonID: 1, TeamID: team.ID, Division: scoring.DivisionExpert,
	})
	if err != nil {
		t.Fatalf("CreateSeasonEntry: %v", err)
	}
	if err := f.store.AddSessionTeam(ctx, f.sess.ID, entry.ID, 0); err != nil {
		t.Fatalf("AddSessionTeam: %v", err)
	}
	second, err := f.store.CreateRound(ctx, f.sess.ID, entry.ID, 1)
	if err != nil {
		t.Fatalf("CreateRound: %v", err)
	}

	rounds, err := f.store.SessionRounds(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("SessionRounds: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("got %d rounds, want 2", len(rounds))
	}
	if rounds[0].ID != second.ID {
		t.Errorf("first round is %d, want %d (running order 0)", rounds[0].ID, second.ID)
	}
}

// The clock reaching zero does not close entry: a throw released before the
// "T" in TIME is still in play.
func TestEnterGrace_KeepsTheRoundOpen(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	if err := f.store.StartRound(ctx, f.round.ID, f.user.ID, time.Now()); err != nil {
		t.Fatalf("StartRound: %v", err)
	}
	if err := f.store.EnterGrace(ctx, f.round.ID, time.Now()); err != nil {
		t.Fatalf("EnterGrace: %v", err)
	}

	r, err := f.store.Round(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	if r.Status != RoundGrace {
		t.Errorf("status = %q, want %q", r.Status, RoundGrace)
	}
	if r.EndedAt == nil {
		t.Error("EndedAt was not stamped")
	}

	// The late throw must still land.
	f.record(t, "buzzer-beater", scoring.Zone40_50, false)
	if got, want := f.total(t), scoring.HalfPoints(10); got != want {
		t.Errorf("total = %v half-points, want %v", got, want)
	}
}

func TestVoidThrow_UnknownThrowIsNotFound(t *testing.T) {
	f := expertFixture(t)

	if err := f.store.VoidThrow(context.Background(), f.round.ID, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("VoidThrow(9999) returned %v, want ErrNotFound", err)
	}
}
