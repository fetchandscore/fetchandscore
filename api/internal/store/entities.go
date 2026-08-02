package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// This file holds the plain record creation and lookup used to stand a league
// up: users, clubs, dogs, teams, seasons and sessions. The interesting
// behaviour lives in round.go.

func (s *Store) CreateUser(ctx context.Context, email, name string) (User, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (email, name, created_at) VALUES (?, ?, ?)`,
		email, name, millis(now))
	if err != nil {
		return User{}, fmt.Errorf("creating user %s: %w", email, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Email: email, Name: name, CreatedAt: now}, nil
}

// UserByEmail looks a user up by address. Matching is case-insensitive, since
// the column collates NOCASE.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, name, created_at FROM users WHERE email = ?`, email,
	).Scan(&u.ID, &u.Email, &u.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("looking up user %s: %w", email, err)
	}
	u.CreatedAt = fromMillis(created)
	return u, nil
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, name, created_at FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Email, &u.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("looking up user %d: %w", id, err)
	}
	u.CreatedAt = fromMillis(created)
	return u, nil
}

func (s *Store) CreateClub(ctx context.Context, slug, name string) (Club, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO clubs (slug, name, created_at) VALUES (?, ?, ?)`,
		slug, name, millis(time.Now()))
	if err != nil {
		return Club{}, fmt.Errorf("creating club %s: %w", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Club{}, err
	}
	return Club{ID: id, Slug: slug, Name: name}, nil
}

func (s *Store) AddClubMember(ctx context.Context, clubID, userID int64, role Role) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO club_members (club_id, user_id, role, joined_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (club_id, user_id) DO UPDATE SET role = excluded.role`,
		clubID, userID, string(role), millis(time.Now()))
	if err != nil {
		return fmt.Errorf("adding user %d to club %d: %w", userID, clubID, err)
	}
	return nil
}

// MemberRole returns a user's role in a club, or ErrNotFound if they are not a
// member. Every club-scoped permission check goes through here.
func (s *Store) MemberRole(ctx context.Context, clubID, userID int64) (Role, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		`SELECT role FROM club_members WHERE club_id = ? AND user_id = ?`,
		clubID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading role of user %d in club %d: %w", userID, clubID, err)
	}
	return Role(role), nil
}

func (s *Store) CreateDog(ctx context.Context, ownerUserID int64, name string, heightCm *float64, tiny bool) (Dog, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO dogs (owner_user_id, name, height_cm, tiny, created_at) VALUES (?, ?, ?, ?, ?)`,
		ownerUserID, name, heightCm, boolToInt(tiny), millis(time.Now()))
	if err != nil {
		return Dog{}, fmt.Errorf("creating dog %s: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Dog{}, err
	}
	return Dog{ID: id, OwnerUserID: ownerUserID, Name: name, HeightCm: heightCm, Tiny: tiny}, nil
}

func (s *Store) CreateTeam(ctx context.Context, clubID, handlerUserID, dogID int64, displayName string) (Team, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO teams (club_id, handler_user_id, dog_id, display_name, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		clubID, handlerUserID, dogID, displayName, millis(time.Now()))
	if err != nil {
		return Team{}, fmt.Errorf("creating team %s: %w", displayName, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Team{}, err
	}
	return Team{
		ID: id, ClubID: clubID, HandlerUserID: handlerUserID,
		DogID: dogID, DisplayName: displayName,
	}, nil
}

// NewSeason describes a season to create. Format supplies the values written
// to the season's own columns; FormatKey records which preset they came from.
type NewSeason struct {
	ClubID    int64
	Name      string
	Year      int
	FormatKey string
	Format    scoring.Format
	WeekCount int
	StartsOn  string
}

func (s *Store) CreateSeason(ctx context.Context, in NewSeason) (Season, error) {
	cues, err := encodeCueSeconds(in.Format.CueSeconds)
	if err != nil {
		return Season{}, fmt.Errorf("encoding cue seconds: %w", err)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO seasons (
			club_id, name, year, format, round_seconds, rounds_per_week,
			scored_throw_cap, tiny_weekly_cap_x2,
			handicap_junior_x2, handicap_handler_x2, handicap_master_x2, handicap_expert_x2,
			cue_seconds, week_count, starts_on, created_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ClubID, in.Name, in.Year, in.FormatKey,
		in.Format.RoundSeconds, in.Format.RoundsPerWeek,
		in.Format.ScoredThrowCap, int(in.Format.TinyWeeklyCap),
		int(in.Format.Handicaps[scoring.DivisionJunior]),
		int(in.Format.Handicaps[scoring.DivisionHandler]),
		int(in.Format.Handicaps[scoring.DivisionMaster]),
		int(in.Format.Handicaps[scoring.DivisionExpert]),
		cues, in.WeekCount, in.StartsOn, millis(time.Now()))
	if err != nil {
		return Season{}, fmt.Errorf("creating season %s: %w", in.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Season{}, err
	}

	return Season{
		ID: id, ClubID: in.ClubID, Name: in.Name, Year: in.Year,
		FormatKey: in.FormatKey, Format: in.Format,
		WeekCount: in.WeekCount, StartsOn: in.StartsOn,
	}, nil
}

// scanSeason reads a season row, rebuilding its Format from the stored columns
// rather than from the preset named by format, so custom values survive.
func scanSeason(row interface{ Scan(...any) error }) (Season, error) {
	var (
		s                      Season
		cues                   string
		junior, handler        int
		master, expert         int
		tinyCap, roundSeconds  int
		roundsPerWeek, capThrw int
	)
	err := row.Scan(
		&s.ID, &s.ClubID, &s.Name, &s.Year, &s.FormatKey,
		&roundSeconds, &roundsPerWeek, &capThrw, &tinyCap,
		&junior, &handler, &master, &expert,
		&cues, &s.WeekCount, &s.StartsOn,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Season{}, ErrNotFound
	}
	if err != nil {
		return Season{}, fmt.Errorf("scanning season: %w", err)
	}

	cueSeconds, err := decodeCueSeconds(cues)
	if err != nil {
		return Season{}, fmt.Errorf("decoding cue seconds for season %d: %w", s.ID, err)
	}

	s.Format = scoring.Format{
		Name:           s.FormatKey,
		RoundSeconds:   roundSeconds,
		RoundsPerWeek:  roundsPerWeek,
		ScoredThrowCap: capThrw,
		TinyWeeklyCap:  scoring.HalfPoints(tinyCap),
		Handicaps: map[scoring.Division]scoring.HalfPoints{
			scoring.DivisionJunior:  scoring.HalfPoints(junior),
			scoring.DivisionHandler: scoring.HalfPoints(handler),
			scoring.DivisionMaster:  scoring.HalfPoints(master),
			scoring.DivisionExpert:  scoring.HalfPoints(expert),
		},
		CueSeconds: cueSeconds,
	}
	return s, nil
}

const seasonColumns = `
	id, club_id, name, year, format,
	round_seconds, rounds_per_week, scored_throw_cap, tiny_weekly_cap_x2,
	handicap_junior_x2, handicap_handler_x2, handicap_master_x2, handicap_expert_x2,
	cue_seconds, week_count, starts_on`

func (s *Store) Season(ctx context.Context, id int64) (Season, error) {
	return scanSeason(s.db.QueryRowContext(ctx,
		`SELECT `+seasonColumns+` FROM seasons WHERE id = ?`, id))
}

// NewSeasonEntry registers a team into a season with the designations that are
// locked for its duration.
type NewSeasonEntry struct {
	SeasonID int64
	TeamID   int64
	Division scoring.Division
	Roller   bool
	Tiny     bool
}

func (s *Store) CreateSeasonEntry(ctx context.Context, in NewSeasonEntry) (SeasonEntry, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO season_entries (season_id, team_id, division, roller, tiny, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		in.SeasonID, in.TeamID, in.Division.String(),
		boolToInt(in.Roller), boolToInt(in.Tiny), millis(time.Now()))
	if err != nil {
		return SeasonEntry{}, fmt.Errorf("registering team %d: %w", in.TeamID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return SeasonEntry{}, err
	}
	return SeasonEntry{
		ID: id, SeasonID: in.SeasonID, TeamID: in.TeamID,
		Division: in.Division, Roller: in.Roller, Tiny: in.Tiny,
	}, nil
}

// NewPlaySession describes a gathering at which rounds are played.
type NewPlaySession struct {
	ClubID   int64
	WeekID   *int64
	Name     string
	StartsAt time.Time
}

func (s *Store) CreatePlaySession(ctx context.Context, in NewPlaySession) (PlaySession, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO play_sessions (club_id, week_id, name, starts_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		in.ClubID, in.WeekID, in.Name, millis(in.StartsAt), millis(time.Now()))
	if err != nil {
		return PlaySession{}, fmt.Errorf("creating play session: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return PlaySession{}, err
	}
	return PlaySession{
		ID: id, ClubID: in.ClubID, WeekID: in.WeekID,
		Name: in.Name, StartsAt: in.StartsAt, Status: "scheduled",
	}, nil
}

func (s *Store) PlaySessionByID(ctx context.Context, id int64) (PlaySession, error) {
	var (
		p        PlaySession
		startsAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, club_id, week_id, name, starts_at, status FROM play_sessions WHERE id = ?`, id,
	).Scan(&p.ID, &p.ClubID, &p.WeekID, &p.Name, &startsAt, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaySession{}, ErrNotFound
	}
	if err != nil {
		return PlaySession{}, fmt.Errorf("looking up play session %d: %w", id, err)
	}
	p.StartsAt = fromMillis(startsAt)
	return p, nil
}

func (s *Store) AddSessionTeam(ctx context.Context, playSessionID, seasonEntryID int64, order int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO play_session_teams (play_session_id, season_entry_id, running_order)
		 VALUES (?, ?, ?)
		 ON CONFLICT (play_session_id, season_entry_id) DO UPDATE SET running_order = excluded.running_order`,
		playSessionID, seasonEntryID, order)
	if err != nil {
		return fmt.Errorf("adding entry %d to session %d: %w", seasonEntryID, playSessionID, err)
	}
	return nil
}
