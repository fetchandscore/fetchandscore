package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// The read models in this file assemble what a screen needs in one query each,
// rather than making a handler stitch rows together.

// SessionTeamView is one team's place in a session's running order, with the
// rounds it has to play.
type SessionTeamView struct {
	SeasonEntryID int64
	TeamName      string
	HandlerName   string
	DogName       string
	Division      scoring.Division
	Tiny          bool
	Roller        bool
	RunningOrder  int
	Rounds        []Round
}

// SessionView is everything the session screen renders.
type SessionView struct {
	Session  PlaySession
	ClubName string
	Format   scoring.Format
	Teams    []SessionTeamView
}

// SessionView assembles a play session with its teams, their designations, and
// every round in running order.
func (s *Store) SessionView(ctx context.Context, sessionID int64) (SessionView, error) {
	session, err := s.PlaySessionByID(ctx, sessionID)
	if err != nil {
		return SessionView{}, err
	}

	var clubName string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM clubs WHERE id = ?`, session.ClubID).Scan(&clubName); err != nil {
		return SessionView{}, fmt.Errorf("reading club of session %d: %w", sessionID, err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT pst.season_entry_id, pst.running_order,
		        t.display_name, u.name, u.email, d.name,
		        e.division, e.tiny, e.roller, e.season_id
		   FROM play_session_teams pst
		   JOIN season_entries e ON e.id = pst.season_entry_id
		   JOIN teams t          ON t.id = e.team_id
		   JOIN users u          ON u.id = t.handler_user_id
		   JOIN dogs d           ON d.id = t.dog_id
		  WHERE pst.play_session_id = ?
		  ORDER BY pst.running_order, t.display_name`, sessionID)
	if err != nil {
		return SessionView{}, fmt.Errorf("listing teams of session %d: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	view := SessionView{Session: session, ClubName: clubName}
	var seasonID int64

	for rows.Next() {
		var (
			team              SessionTeamView
			division          string
			tiny, roller      int
			handlerEmail      string
			entrySeasonID     int64
			handlerNameColumn string
		)
		if err := rows.Scan(
			&team.SeasonEntryID, &team.RunningOrder,
			&team.TeamName, &handlerNameColumn, &handlerEmail, &team.DogName,
			&division, &tiny, &roller, &entrySeasonID,
		); err != nil {
			return SessionView{}, fmt.Errorf("scanning session team: %w", err)
		}

		parsed, err := scoring.ParseDivision(division)
		if err != nil {
			return SessionView{}, err
		}
		team.Division = parsed
		team.Tiny = tiny == 1
		team.Roller = roller == 1

		// A user who has signed in but never set a name is shown by the local
		// part of their address rather than as a blank.
		team.HandlerName = handlerNameColumn
		if team.HandlerName == "" {
			team.HandlerName = localPart(handlerEmail)
		}

		seasonID = entrySeasonID
		view.Teams = append(view.Teams, team)
	}
	if err := rows.Err(); err != nil {
		return SessionView{}, err
	}

	if seasonID != 0 {
		season, err := s.Season(ctx, seasonID)
		if err != nil {
			return SessionView{}, err
		}
		view.Format = season.Format
	}

	rounds, err := s.SessionRounds(ctx, sessionID)
	if err != nil {
		return SessionView{}, err
	}
	byEntry := make(map[int64][]Round, len(view.Teams))
	for _, r := range rounds {
		byEntry[r.SeasonEntryID] = append(byEntry[r.SeasonEntryID], r)
	}
	for i := range view.Teams {
		view.Teams[i].Rounds = byEntry[view.Teams[i].SeasonEntryID]
	}

	return view, nil
}

// SessionSummary is a session as it appears in a list.
type SessionSummary struct {
	ID        int64
	ClubID    int64
	ClubName  string
	Name      string
	StartsAt  time.Time
	Status    string
	TeamCount int
}

// Dashboard is the landing screen: what is happening now, what is next, and
// what already happened.
type Dashboard struct {
	Active   []SessionSummary
	Upcoming []SessionSummary
	Past     []SessionSummary
}

// activeWindow is how long after its start time a session still counts as
// happening now. A league night runs an hour or two; six hours is generous
// without letting a forgotten session sit "active" for days.
const activeWindow = 6 * time.Hour

// Dashboard returns the sessions of every club the user belongs to, sorted
// into what is live, what is coming, and what is done.
func (s *Store) Dashboard(ctx context.Context, userID int64, now time.Time) (Dashboard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ps.id, ps.club_id, c.name, ps.name, ps.starts_at, ps.status,
		        (SELECT count(*) FROM play_session_teams pst WHERE pst.play_session_id = ps.id)
		   FROM play_sessions ps
		   JOIN clubs c        ON c.id = ps.club_id
		   JOIN club_members m ON m.club_id = ps.club_id
		  WHERE m.user_id = ? AND ps.status != 'cancelled'
		  ORDER BY ps.starts_at DESC`, userID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("listing sessions for user %d: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var out Dashboard
	for rows.Next() {
		var (
			sum      SessionSummary
			startsAt int64
		)
		if err := rows.Scan(&sum.ID, &sum.ClubID, &sum.ClubName, &sum.Name,
			&startsAt, &sum.Status, &sum.TeamCount); err != nil {
			return Dashboard{}, fmt.Errorf("scanning session summary: %w", err)
		}
		sum.StartsAt = fromMillis(startsAt)

		switch {
		case sum.Status == "complete":
			out.Past = append(out.Past, sum)
		case sum.Status == "active":
			out.Active = append(out.Active, sum)
		case sum.StartsAt.After(now):
			out.Upcoming = append(out.Upcoming, sum)
		case now.Sub(sum.StartsAt) < activeWindow:
			// Started recently and nobody marked it active yet: it is tonight's
			// session, and burying it under "past" would be useless.
			out.Active = append(out.Active, sum)
		default:
			out.Past = append(out.Past, sum)
		}
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, err
	}

	// Upcoming reads best soonest-first; the query sorted newest-first.
	reverse(out.Upcoming)
	return out, nil
}

func reverse(xs []SessionSummary) {
	for i, j := 0, len(xs)-1; i < j; i, j = i+1, j-1 {
		xs[i], xs[j] = xs[j], xs[i]
	}
}

// RoundLocation returns the club and play session a round belongs to, which is
// what every round-scoped permission check needs.
func (s *Store) RoundLocation(ctx context.Context, roundID int64) (clubID, sessionID int64, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT ps.club_id, ps.id
		   FROM rounds r
		   JOIN play_sessions ps ON ps.id = r.play_session_id
		  WHERE r.id = ?`, roundID).Scan(&clubID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("round %d: %w", roundID, ErrNotFound)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("locating round %d: %w", roundID, err)
	}
	return clubID, sessionID, nil
}

// localPart is the part of an address before the @, used as a fallback display
// name.
func localPart(email string) string {
	for i, r := range email {
		if r == '@' {
			return email[:i]
		}
	}
	return email
}
