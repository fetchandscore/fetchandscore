package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// Columns are qualified because SessionRounds joins rounds against
// play_session_teams, which shares two of these names.
const roundColumns = `
	r.id, r.play_session_id, r.season_entry_id, r.round_number, r.status,
	r.started_at, r.ended_at, r.confirmed_at, r.scorekeeper_user_id, r.total_x2`

func scanRound(row interface{ Scan(...any) error }) (Round, error) {
	var (
		r                            Round
		status                       string
		started, ended, confirmed    *int64
		scorekeeper                  *int64
		total                        int
		playSessionID, seasonEntryID int64
		roundNumber                  int
	)
	err := row.Scan(
		&r.ID, &playSessionID, &seasonEntryID, &roundNumber, &status,
		&started, &ended, &confirmed, &scorekeeper, &total,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Round{}, ErrNotFound
	}
	if err != nil {
		return Round{}, fmt.Errorf("scanning round: %w", err)
	}

	r.PlaySessionID = playSessionID
	r.SeasonEntryID = seasonEntryID
	r.Number = roundNumber
	r.Status = RoundStatus(status)
	r.StartedAt = nullableTime(started)
	r.EndedAt = nullableTime(ended)
	r.ConfirmedAt = nullableTime(confirmed)
	r.ScorekeeperUserID = scorekeeper
	r.TotalX2 = scoring.HalfPoints(total)
	return r, nil
}

func (s *Store) CreateRound(ctx context.Context, playSessionID, seasonEntryID int64, number int) (Round, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO rounds (play_session_id, season_entry_id, round_number) VALUES (?, ?, ?)`,
		playSessionID, seasonEntryID, number)
	if err != nil {
		return Round{}, fmt.Errorf("creating round %d: %w", number, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Round{}, err
	}
	return Round{
		ID: id, PlaySessionID: playSessionID, SeasonEntryID: seasonEntryID,
		Number: number, Status: RoundReady,
	}, nil
}

func (s *Store) Round(ctx context.Context, id int64) (Round, error) {
	return scanRound(s.db.QueryRowContext(ctx,
		`SELECT `+roundColumns+` FROM rounds r WHERE r.id = ?`, id))
}

// SessionRounds returns every round of a play session in running order.
func (s *Store) SessionRounds(ctx context.Context, playSessionID int64) ([]Round, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+roundColumns+`
		 FROM rounds r
		 JOIN play_session_teams t
		   ON t.play_session_id = r.play_session_id
		  AND t.season_entry_id = r.season_entry_id
		 WHERE r.play_session_id = ?
		 ORDER BY t.running_order, r.round_number`, playSessionID)
	if err != nil {
		return nil, fmt.Errorf("listing rounds of session %d: %w", playSessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Round
	for rows.Next() {
		r, err := scanRound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartRound moves a round to running, stamping the clock and recording who is
// keeping score.
func (s *Store) StartRound(ctx context.Context, roundID, scorekeeperUserID int64, at time.Time) error {
	return s.updateRound(ctx, roundID,
		`UPDATE rounds SET status = ?, started_at = ?, scorekeeper_user_id = ? WHERE id = ?`,
		string(RoundRunning), millis(at), scorekeeperUserID, roundID)
}

// EnterGrace marks the clock as expired while leaving entry open, because a
// throw released before the "T" in TIME is still in play.
func (s *Store) EnterGrace(ctx context.Context, roundID int64, at time.Time) error {
	return s.updateRound(ctx, roundID,
		`UPDATE rounds SET status = ?, ended_at = ? WHERE id = ?`,
		string(RoundGrace), millis(at), roundID)
}

// ConfirmRound locks a round as the official score.
func (s *Store) ConfirmRound(ctx context.Context, roundID int64, at time.Time) error {
	return s.updateRound(ctx, roundID,
		`UPDATE rounds SET status = ?, confirmed_at = ?, ended_at = COALESCE(ended_at, ?) WHERE id = ?`,
		string(RoundConfirmed), millis(at), millis(at), roundID)
}

// ResetRound returns a round to its pre-start state after a false start.
//
// The throws are deleted rather than voided: a false start means the round did
// not happen, so there is nothing to audit, and the replayed round will reuse
// the same client ids.
func (s *Store) ResetRound(ctx context.Context, roundID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning reset of round %d: %w", roundID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM throws WHERE round_id = ?`, roundID); err != nil {
		return fmt.Errorf("clearing throws of round %d: %w", roundID, err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE rounds
		    SET status = ?, started_at = NULL, ended_at = NULL, confirmed_at = NULL, total_x2 = 0
		  WHERE id = ?`,
		string(RoundReady), roundID)
	if err != nil {
		return fmt.Errorf("resetting round %d: %w", roundID, err)
	}
	if err := requireAffected(res, roundID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) updateRound(ctx context.Context, roundID int64, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("updating round %d: %w", roundID, err)
	}
	return requireAffected(res, roundID)
}

func requireAffected(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("round %d: %w", id, ErrNotFound)
	}
	return nil
}

// NewThrow is a throw as the client reports it.
type NewThrow struct {
	// ClientID is generated on the device. It is what makes the write
	// idempotent: a retry after an ambiguous timeout carries the same id.
	ClientID string
	Zone     scoring.Zone
	Air      bool
	// RecordedAt is the client's clock at the moment of the tap, which is the
	// truthful ordering when a queued batch drains after a dropout.
	RecordedAt time.Time
}

// AddThrow records a throw and returns it, then refreshes the round's cached
// total.
//
// Re-sending a client id that already exists returns the stored throw
// unchanged rather than adding a second one, so a retry can never double-score.
func (s *Store) AddThrow(ctx context.Context, roundID int64, in NewThrow) (Throw, error) {
	round, err := s.Round(ctx, roundID)
	if err != nil {
		return Throw{}, err
	}
	if round.Status == RoundConfirmed {
		return Throw{}, fmt.Errorf("round %d: %w", roundID, ErrRoundClosed)
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO throws (round_id, zone, air, client_id, recorded_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (round_id, client_id) DO NOTHING`,
		roundID, in.Zone.String(), boolToInt(in.Air), in.ClientID,
		millis(in.RecordedAt), millis(time.Now()))
	if err != nil {
		return Throw{}, fmt.Errorf("recording throw on round %d: %w", roundID, err)
	}

	throw, err := s.throwByClientID(ctx, roundID, in.ClientID)
	if err != nil {
		return Throw{}, err
	}
	if _, err := s.RecomputeRoundTotal(ctx, roundID); err != nil {
		return Throw{}, err
	}
	return throw, nil
}

// VoidThrow marks a throw as undone. The row survives so a disputed round
// keeps its trail.
func (s *Store) VoidThrow(ctx context.Context, roundID, throwID int64) error {
	round, err := s.Round(ctx, roundID)
	if err != nil {
		return err
	}
	if round.Status == RoundConfirmed {
		return fmt.Errorf("round %d: %w", roundID, ErrRoundClosed)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE throws SET void = 1 WHERE id = ? AND round_id = ?`, throwID, roundID)
	if err != nil {
		return fmt.Errorf("voiding throw %d: %w", throwID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("throw %d on round %d: %w", throwID, roundID, ErrNotFound)
	}

	_, err = s.RecomputeRoundTotal(ctx, roundID)
	return err
}

const throwColumns = `id, round_id, zone, air, client_id, void, recorded_at`

func scanThrow(row interface{ Scan(...any) error }) (Throw, error) {
	var (
		t          Throw
		zone       string
		air, void  int
		recordedAt int64
	)
	err := row.Scan(&t.ID, &t.RoundID, &zone, &air, &t.ClientID, &void, &recordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Throw{}, ErrNotFound
	}
	if err != nil {
		return Throw{}, fmt.Errorf("scanning throw: %w", err)
	}

	parsed, err := scoring.ParseZone(zone)
	if err != nil {
		return Throw{}, fmt.Errorf("throw %d: %w", t.ID, err)
	}
	t.Zone = parsed
	t.Air = air == 1
	t.Void = void == 1
	t.RecordedAt = fromMillis(recordedAt)
	return t, nil
}

func (s *Store) throwByClientID(ctx context.Context, roundID int64, clientID string) (Throw, error) {
	return scanThrow(s.db.QueryRowContext(ctx,
		`SELECT `+throwColumns+` FROM throws WHERE round_id = ? AND client_id = ?`,
		roundID, clientID))
}

// RoundThrows returns every throw of a round in the order it was tapped,
// including voided ones so the caller can show the trail.
func (s *Store) RoundThrows(ctx context.Context, roundID int64) ([]Throw, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+throwColumns+` FROM throws WHERE round_id = ? ORDER BY recorded_at, id`, roundID)
	if err != nil {
		return nil, fmt.Errorf("listing throws of round %d: %w", roundID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Throw
	for rows.Next() {
		t, err := scanThrow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// roundScoringContext fetches the team designations and season format a round
// must be scored under. Reading them per round rather than caching them means
// a correction to a season's format takes effect on the next write.
func (s *Store) roundScoringContext(ctx context.Context, roundID int64) (scoring.Flags, scoring.Format, error) {
	var (
		division string
		roller   int
		tiny     int
		seasonID int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT e.division, e.roller, e.tiny, e.season_id
		   FROM rounds r
		   JOIN season_entries e ON e.id = r.season_entry_id
		  WHERE r.id = ?`, roundID,
	).Scan(&division, &roller, &tiny, &seasonID)
	if errors.Is(err, sql.ErrNoRows) {
		return scoring.Flags{}, scoring.Format{}, fmt.Errorf("round %d: %w", roundID, ErrNotFound)
	}
	if err != nil {
		return scoring.Flags{}, scoring.Format{}, fmt.Errorf("reading scoring context of round %d: %w", roundID, err)
	}

	parsed, err := scoring.ParseDivision(division)
	if err != nil {
		return scoring.Flags{}, scoring.Format{}, fmt.Errorf("round %d: %w", roundID, err)
	}

	season, err := s.Season(ctx, seasonID)
	if err != nil {
		return scoring.Flags{}, scoring.Format{}, err
	}

	flags := scoring.Flags{Tiny: tiny == 1, Roller: roller == 1, Division: parsed}
	return flags, season.Format, nil
}

// RecomputeRoundTotal rescores a round from its throws and caches the result.
//
// The throws remain the source of truth; total_x2 exists so a session list can
// be rendered without rescoring every round.
func (s *Store) RecomputeRoundTotal(ctx context.Context, roundID int64) (scoring.HalfPoints, error) {
	flags, format, err := s.roundScoringContext(ctx, roundID)
	if err != nil {
		return 0, err
	}

	stored, err := s.RoundThrows(ctx, roundID)
	if err != nil {
		return 0, err
	}

	live := make([]scoring.Throw, 0, len(stored))
	for _, t := range stored {
		if t.Void {
			continue
		}
		live = append(live, scoring.Throw{Zone: t.Zone, Air: t.Air})
	}

	total := scoring.ScoreRound(live, flags, format)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE rounds SET total_x2 = ? WHERE id = ?`, int(total), roundID); err != nil {
		return 0, fmt.Errorf("caching total of round %d: %w", roundID, err)
	}
	return total, nil
}
