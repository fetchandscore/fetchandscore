package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

func TestSessionView_AssemblesTeamsRoundsAndFormat(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, scoring.Format90Top5(), "90_5", scoring.Flags{
		Tiny: true, Division: scoring.DivisionMaster,
	})

	// A second round for the same team, as a league week actually has.
	if _, err := f.store.CreateRound(ctx, f.sess.ID, f.entry.ID, 2); err != nil {
		t.Fatalf("CreateRound: %v", err)
	}

	view, err := f.store.SessionView(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("SessionView: %v", err)
	}

	if view.ClubName != "Test Club" {
		t.Errorf("ClubName = %q, want %q", view.ClubName, "Test Club")
	}
	if view.Format.ScoredThrowCap != 5 {
		t.Errorf("format cap = %d, want the season's 5", view.Format.ScoredThrowCap)
	}
	if len(view.Teams) != 1 {
		t.Fatalf("got %d teams, want 1", len(view.Teams))
	}

	team := view.Teams[0]
	if team.TeamName != "Sadie · Brad" || team.DogName != "Sadie" {
		t.Errorf("team = %+v, want the seeded names", team)
	}
	if !team.Tiny || team.Division != scoring.DivisionMaster {
		t.Errorf("designations = tiny:%v division:%v, want tiny:true master", team.Tiny, team.Division)
	}
	if len(team.Rounds) != 2 {
		t.Errorf("team has %d rounds, want 2", len(team.Rounds))
	}
}

// A handler who has signed in but never set a name should not render blank.
func TestSessionView_FallsBackToTheEmailLocalPart(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	view, err := f.store.SessionView(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("SessionView: %v", err)
	}
	if view.Teams[0].HandlerName != "Brad" {
		t.Errorf("HandlerName = %q, want the stored name", view.Teams[0].HandlerName)
	}

	if _, err := f.store.DB().ExecContext(ctx, `UPDATE users SET name = '' WHERE id = ?`, f.user.ID); err != nil {
		t.Fatalf("clearing name: %v", err)
	}
	view, err = f.store.SessionView(ctx, f.sess.ID)
	if err != nil {
		t.Fatalf("SessionView: %v", err)
	}
	if view.Teams[0].HandlerName != "handler" {
		t.Errorf("HandlerName = %q, want the email local part %q", view.Teams[0].HandlerName, "handler")
	}
}

func TestSessionView_UnknownSessionIsNotFound(t *testing.T) {
	s := openTemp(t)

	if _, err := s.SessionView(context.Background(), 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionView(9999) returned %v, want ErrNotFound", err)
	}
}

func TestDashboard_SortsSessionsByWhenTheyHappen(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)
	now := time.Now()

	mk := func(name string, at time.Time, status string) int64 {
		sess, err := f.store.CreatePlaySession(ctx, NewPlaySession{
			ClubID: f.club.ID, Name: name, StartsAt: at,
		})
		if err != nil {
			t.Fatalf("CreatePlaySession(%s): %v", name, err)
		}
		if status != "scheduled" {
			if _, err := f.store.DB().ExecContext(ctx,
				`UPDATE play_sessions SET status = ? WHERE id = ?`, status, sess.ID); err != nil {
				t.Fatalf("setting status: %v", err)
			}
		}
		return sess.ID
	}

	nextWeek := mk("Next week", now.Add(7*24*time.Hour), "scheduled")
	tomorrow := mk("Tomorrow", now.Add(24*time.Hour), "scheduled")
	longAgo := mk("Long ago", now.Add(-30*24*time.Hour), "scheduled")
	finished := mk("Finished", now.Add(-time.Hour), "complete")
	running := mk("Running", now.Add(-30*time.Minute), "active")
	mk("Cancelled", now.Add(24*time.Hour), "cancelled")

	dash, err := f.store.Dashboard(ctx, f.user.ID, now)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	ids := func(xs []SessionSummary) []int64 {
		out := make([]int64, len(xs))
		for i, x := range xs {
			out[i] = x.ID
		}
		return out
	}

	// The fixture's own session starts now, so it counts as active too.
	activeIDs := ids(dash.Active)
	if !contains(activeIDs, running) || !contains(activeIDs, f.sess.ID) {
		t.Errorf("Active = %v, want it to include the running and just-started sessions", activeIDs)
	}
	if contains(activeIDs, finished) {
		t.Error("a completed session was listed as active")
	}

	// Upcoming reads soonest first.
	if got := ids(dash.Upcoming); len(got) != 2 || got[0] != tomorrow || got[1] != nextWeek {
		t.Errorf("Upcoming = %v, want [tomorrow=%d nextWeek=%d]", got, tomorrow, nextWeek)
	}

	pastIDs := ids(dash.Past)
	if !contains(pastIDs, longAgo) || !contains(pastIDs, finished) {
		t.Errorf("Past = %v, want it to include the old and completed sessions", pastIDs)
	}

	// A cancelled session appears nowhere.
	for _, group := range [][]SessionSummary{dash.Active, dash.Upcoming, dash.Past} {
		for _, s := range group {
			if s.Name == "Cancelled" {
				t.Error("a cancelled session was listed")
			}
		}
	}
}

// The dashboard is scoped to the clubs you belong to, not to every club on the
// server.
func TestDashboard_ExcludesOtherClubs(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	other, err := f.store.CreateClub(ctx, "other", "Other Club")
	if err != nil {
		t.Fatalf("CreateClub: %v", err)
	}
	if _, err := f.store.CreatePlaySession(ctx, NewPlaySession{
		ClubID: other.ID, Name: "Not mine", StartsAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreatePlaySession: %v", err)
	}

	dash, err := f.store.Dashboard(ctx, f.user.ID, time.Now())
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}

	for _, group := range [][]SessionSummary{dash.Active, dash.Upcoming, dash.Past} {
		for _, s := range group {
			if s.Name == "Not mine" {
				t.Fatal("dashboard leaked a session from a club the user is not in")
			}
		}
	}
}

func TestDashboard_CountsTeams(t *testing.T) {
	f := expertFixture(t)

	dash, err := f.store.Dashboard(context.Background(), f.user.ID, time.Now())
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	for _, s := range dash.Active {
		if s.ID == f.sess.ID && s.TeamCount != 1 {
			t.Errorf("TeamCount = %d, want 1", s.TeamCount)
		}
	}
}

func TestRoundLocation(t *testing.T) {
	ctx := context.Background()
	f := expertFixture(t)

	clubID, sessionID, err := f.store.RoundLocation(ctx, f.round.ID)
	if err != nil {
		t.Fatalf("RoundLocation: %v", err)
	}
	if clubID != f.club.ID || sessionID != f.sess.ID {
		t.Errorf("RoundLocation = club %d session %d, want club %d session %d",
			clubID, sessionID, f.club.ID, f.sess.ID)
	}

	if _, _, err := f.store.RoundLocation(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("RoundLocation(9999) returned %v, want ErrNotFound", err)
	}
}

func contains(xs []int64, want int64) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
