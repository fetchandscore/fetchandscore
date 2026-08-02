package store

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
)

// ErrNotFound is returned when a lookup by id matches nothing.
var ErrNotFound = errors.New("not found")

// ErrRoundClosed is returned when a write targets a confirmed round. A
// confirmed round is the official score and does not accept silent appends.
var ErrRoundClosed = errors.New("round is confirmed")

// Role is a member's authority within a club.
type Role string

const (
	RoleMember      Role = "member"
	RoleScorekeeper Role = "scorekeeper"
	RoleCaptain     Role = "captain"
	RoleAdmin       Role = "admin"
)

// rank orders roles so a permission check can ask for "captain or better".
var roleRank = map[Role]int{
	RoleMember:      0,
	RoleScorekeeper: 1,
	RoleCaptain:     2,
	RoleAdmin:       3,
}

// AtLeast reports whether r carries at least the authority of min.
func (r Role) AtLeast(min Role) bool { return roleRank[r] >= roleRank[min] }

// RoundStatus tracks a round through its lifecycle.
//
// The grace state exists because the rules count a throw released before the
// "T" in TIME: the clock reaches zero but entry stays open until a human
// confirms the round is done.
type RoundStatus string

const (
	RoundReady     RoundStatus = "ready"
	RoundRunning   RoundStatus = "running"
	RoundGrace     RoundStatus = "grace"
	RoundConfirmed RoundStatus = "confirmed"
)

type User struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

type Club struct {
	ID   int64
	Slug string
	Name string
}

type Dog struct {
	ID          int64
	OwnerUserID int64
	Name        string
	HeightCm    *float64
	Tiny        bool
}

type Team struct {
	ID            int64
	ClubID        int64
	HandlerUserID int64
	DogID         int64
	DisplayName   string
}

// Season carries its own format values rather than a reference to a preset, so
// a club running a custom round length keeps working after the presets change.
type Season struct {
	ID        int64
	ClubID    int64
	Name      string
	Year      int
	FormatKey string
	Format    scoring.Format
	WeekCount int
	StartsOn  string
}

type SeasonEntry struct {
	ID       int64
	SeasonID int64
	TeamID   int64
	Division scoring.Division
	Roller   bool
	Tiny     bool
}

// Flags returns the entry's designations in the shape the rules engine wants.
func (e SeasonEntry) Flags() scoring.Flags {
	return scoring.Flags{Tiny: e.Tiny, Roller: e.Roller, Division: e.Division}
}

type PlaySession struct {
	ID       int64
	ClubID   int64
	WeekID   *int64
	Name     string
	StartsAt time.Time
	Status   string
}

type Round struct {
	ID                int64
	PlaySessionID     int64
	SeasonEntryID     int64
	Number            int
	Status            RoundStatus
	StartedAt         *time.Time
	EndedAt           *time.Time
	ConfirmedAt       *time.Time
	ScorekeeperUserID *int64
	TotalX2           scoring.HalfPoints
}

type Throw struct {
	ID         int64
	RoundID    int64
	Zone       scoring.Zone
	Air        bool
	ClientID   string
	Void       bool
	RecordedAt time.Time
}

// millis converts a time to the Unix-millisecond integers the schema stores.
func millis(t time.Time) int64 { return t.UTC().UnixMilli() }

// fromMillis is the inverse of millis.
func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// nullableTime converts a nullable millisecond column to a time.
func nullableTime(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := fromMillis(*ms)
	return &t
}

// boolToInt renders a Go bool as the 0/1 the CHECK constraints expect.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// encodeCueSeconds stores the timer's speaking marks as a JSON array.
func encodeCueSeconds(cues []int) (string, error) {
	b, err := json.Marshal(cues)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeCueSeconds(s string) ([]int, error) {
	var cues []int
	if err := json.Unmarshal([]byte(s), &cues); err != nil {
		return nil, err
	}
	return cues, nil
}
