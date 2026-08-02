package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// fixture is a club with one registered team and one round ready to score,
// which is the minimum state in which the round API means anything.
type fixture struct {
	store *Store
	user  User
	club  Club
	entry SeasonEntry
	sess  PlaySession
	round Round
}

func newFixture(t *testing.T, format scoring.Format, formatKey string, flags scoring.Flags) fixture {
	t.Helper()
	ctx := context.Background()
	s := openTemp(t)

	user, err := s.CreateUser(ctx, "handler@example.com", "Brad")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	club, err := s.CreateClub(ctx, "test-club", "Test Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	if err := s.AddClubMember(ctx, club.ID, user.ID, RoleCaptain); err != nil {
		t.Fatalf("AddClubMember: %v", err)
	}
	dog, err := s.CreateDog(ctx, user.ID, "Sadie", nil, flags.Tiny)
	if err != nil {
		t.Fatalf("CreateDog: %v", err)
	}
	team, err := s.CreateTeam(ctx, club.ID, user.ID, dog.ID, "Sadie · Brad")
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	season, err := s.CreateSeason(ctx, NewSeason{
		ClubID:    club.ID,
		Name:      "Test Season",
		Year:      2026,
		FormatKey: formatKey,
		Format:    format,
		WeekCount: 5,
		StartsOn:  "2026-08-01",
	})
	if err != nil {
		t.Fatalf("CreateSeason: %v", err)
	}
	entry, err := s.CreateSeasonEntry(ctx, NewSeasonEntry{
		SeasonID: season.ID,
		TeamID:   team.ID,
		Division: flags.Division,
		Roller:   flags.Roller,
		Tiny:     flags.Tiny,
	})
	if err != nil {
		t.Fatalf("CreateSeasonEntry: %v", err)
	}
	sess, err := s.CreatePlaySession(ctx, NewPlaySession{
		ClubID:   club.ID,
		Name:     "Week 1",
		StartsAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}
	if err := s.AddSessionTeam(ctx, sess.ID, entry.ID, 1); err != nil {
		t.Fatalf("AddSessionTeam: %v", err)
	}
	round, err := s.CreateRound(ctx, sess.ID, entry.ID, 1)
	if err != nil {
		t.Fatalf("CreateRound: %v", err)
	}

	return fixture{store: s, user: user, club: club, entry: entry, sess: sess, round: round}
}

func expertFixture(t *testing.T) fixture {
	t.Helper()
	return newFixture(t, scoring.Format60All(), "60_all", scoring.Flags{Division: scoring.DivisionExpert})
}

// record adds a throw with a fresh client id, as the UI would.
func (f fixture) record(t *testing.T, clientID string, zone scoring.Zone, air bool) Throw {
	t.Helper()
	th, err := f.store.AddThrow(context.Background(), f.round.ID, NewThrow{
		ClientID:   clientID,
		Zone:       zone,
		Air:        air,
		RecordedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("AddThrow(%s): %v", clientID, err)
	}
	return th
}

func (f fixture) total(t *testing.T) scoring.HalfPoints {
	t.Helper()
	r, err := f.store.Round(context.Background(), f.round.ID)
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	return r.TotalX2
}

func TestAddThrow_UpdatesTheCachedRoundTotal(t *testing.T) {
	f := expertFixture(t)

	f.record(t, "a", scoring.Zone30_40, false) // 3
	f.record(t, "b", scoring.Zone40_50, true)  // 5.5
	f.record(t, "c", scoring.ZoneMiss, false)  // 0

	if got, want := f.total(t), scoring.HalfPoints(17); got != want {
		t.Errorf("round total = %v half-points, want %v", got, want)
	}
}

// The client generates the id, so a retry after an ambiguous timeout resends
// the same one. That must never score twice.
func TestAddThrow_IsIdempotentOnClientID(t *testing.T) {
	f := expertFixture(t)

	first := f.record(t, "same-id", scoring.Zone40_50, false)
	second := f.record(t, "same-id", scoring.Zone40_50, false)

	if first.ID != second.ID {
		t.Errorf("retry created throw %d, want the original %d", second.ID, first.ID)
	}
	if got, want := f.total(t), scoring.HalfPoints(10); got != want {
		t.Errorf("round total = %v half-points, want %v (the throw counted twice)", got, want)
	}

	throws, err := f.store.RoundThrows(context.Background(), f.round.ID)
	if err != nil {
		t.Fatalf("RoundThrows: %v", err)
	}
	if len(throws) != 1 {
		t.Errorf("round holds %d throws, want 1", len(throws))
	}
}

func TestVoidThrow_RemovesItFromTheTotal(t *testing.T) {
	f := expertFixture(t)

	f.record(t, "a", scoring.Zone40_50, false) // 5
	mistake := f.record(t, "b", scoring.Zone40_50, false)

	if err := f.store.VoidThrow(context.Background(), f.round.ID, mistake.ID); err != nil {
		t.Fatalf("VoidThrow: %v", err)
	}

	if got, want := f.total(t), scoring.HalfPoints(10); got != want {
		t.Errorf("round total = %v half-points, want %v", got, want)
	}
}

// Undo is a soft void: the row survives so a disputed round has a trail.
func TestVoidThrow_KeepsTheRowForAudit(t *testing.T) {
	f := expertFixture(t)
	mistake := f.record(t, "a", scoring.Zone40_50, false)

	if err := f.store.VoidThrow(context.Background(), f.round.ID, mistake.ID); err != nil {
		t.Fatalf("VoidThrow: %v", err)
	}

	throws, err := f.store.RoundThrows(context.Background(), f.round.ID)
	if err != nil {
		t.Fatalf("RoundThrows: %v", err)
	}
	if len(throws) != 1 {
		t.Fatalf("round holds %d throws, want the voided one retained", len(throws))
	}
	if !throws[0].Void {
		t.Error("throw is not marked void")
	}
}

// The cached total must be computed with the season's own format, not a
// default, or a 90:/5 round would silently score every catch.
func TestAddThrow_AppliesTheSeasonFormat(t *testing.T) {
	f := newFixture(t, scoring.Format90Top5(), "90_5", scoring.Flags{Division: scoring.DivisionExpert})

	for i, z := range []scoring.Zone{
		scoring.Zone40_50, scoring.Zone40_50, scoring.Zone30_40,
		scoring.Zone30_40, scoring.Zone20_30, scoring.Zone10_20,
	} {
		f.record(t, string(rune('a'+i)), z, false)
	}

	// 5 + 5 + 3 + 3 + 2; the 1-point catch falls outside the top five.
	if got, want := f.total(t), scoring.HalfPoints(36); got != want {
		t.Errorf("round total = %v half-points, want %v", got, want)
	}
}

// The team's designations come from its season entry, not from defaults.
func TestAddThrow_AppliesTeamFlags(t *testing.T) {
	f := newFixture(t, scoring.Format60All(), "60_all", scoring.Flags{
		Tiny: true, Division: scoring.DivisionExpert,
	})

	f.record(t, "a", scoring.Zone30_40, false) // 3 + 1 tiny = 4

	if got, want := f.total(t), scoring.HalfPoints(8); got != want {
		t.Errorf("round total = %v half-points, want %v", got, want)
	}
}

func TestRoundLifecycle(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	if f.round.Status != RoundReady {
		t.Fatalf("new round status = %q, want %q", f.round.Status, RoundReady)
	}

	if err := f.store.StartRound(ctx, f.round.ID, f.user.ID, time.Now()); err != nil {
		t.Fatalf("StartRound: %v", err)
	}
	r, err := f.store.Round(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	if r.Status != RoundRunning {
		t.Errorf("status after start = %q, want %q", r.Status, RoundRunning)
	}
	if r.StartedAt == nil {
		t.Error("StartedAt was not stamped")
	}
	if r.ScorekeeperUserID == nil || *r.ScorekeeperUserID != f.user.ID {
		t.Error("scorekeeper was not recorded")
	}

	f.record(t, "a", scoring.Zone40_50, false)

	if err := f.store.ConfirmRound(ctx, f.round.ID, time.Now()); err != nil {
		t.Fatalf("ConfirmRound: %v", err)
	}
	r, err = f.store.Round(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	if r.Status != RoundConfirmed {
		t.Errorf("status after confirm = %q, want %q", r.Status, RoundConfirmed)
	}
	if r.ConfirmedAt == nil {
		t.Error("ConfirmedAt was not stamped")
	}
}

// A confirmed round is the official score. Nothing may append to it silently.
func TestAddThrow_RejectedAfterConfirm(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	if err := f.store.StartRound(ctx, f.round.ID, f.user.ID, time.Now()); err != nil {
		t.Fatalf("StartRound: %v", err)
	}
	if err := f.store.ConfirmRound(ctx, f.round.ID, time.Now()); err != nil {
		t.Fatalf("ConfirmRound: %v", err)
	}

	_, err := f.store.AddThrow(ctx, f.round.ID, NewThrow{
		ClientID:   "late",
		Zone:       scoring.Zone40_50,
		RecordedAt: time.Now(),
	})
	if !errors.Is(err, ErrRoundClosed) {
		t.Fatalf("AddThrow after confirm returned %v, want ErrRoundClosed", err)
	}
}

// "Early movement = False Start. The timer resets, and the round starts over."
func TestResetRound_ClearsThrowsAndReturnsToReady(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	if err := f.store.StartRound(ctx, f.round.ID, f.user.ID, time.Now()); err != nil {
		t.Fatalf("StartRound: %v", err)
	}
	f.record(t, "a", scoring.Zone40_50, false)

	if err := f.store.ResetRound(ctx, f.round.ID); err != nil {
		t.Fatalf("ResetRound: %v", err)
	}

	r, err := f.store.Round(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("Round: %v", err)
	}
	if r.Status != RoundReady {
		t.Errorf("status after reset = %q, want %q", r.Status, RoundReady)
	}
	if r.StartedAt != nil {
		t.Error("StartedAt survived the reset")
	}
	if r.TotalX2 != 0 {
		t.Errorf("total after reset = %v, want 0", r.TotalX2)
	}

	throws, err := f.store.RoundThrows(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("RoundThrows: %v", err)
	}
	if len(throws) != 0 {
		t.Errorf("round still holds %d throws after a false start", len(throws))
	}
}

// A false start reuses the same client ids on the replayed round, so the
// unique constraint must have been cleared along with the throws.
func TestResetRound_FreesClientIDsForReuse(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	f.record(t, "a", scoring.Zone10_20, false)
	if err := f.store.ResetRound(ctx, f.round.ID); err != nil {
		t.Fatalf("ResetRound: %v", err)
	}
	f.record(t, "a", scoring.Zone40_50, false)

	if got, want := f.total(t), scoring.HalfPoints(10); got != want {
		t.Errorf("round total = %v half-points, want %v", got, want)
	}
}

func TestRound_UnknownIDIsNotFound(t *testing.T) {
	s := openTemp(t)

	_, err := s.Round(context.Background(), 12345)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Round(12345) returned %v, want ErrNotFound", err)
	}
}
