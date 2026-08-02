// Package scoring implements the K9 Frisbee Toss & Fetch league scoring rules.
//
// It is deliberately pure: no database, no HTTP, no clock. Everything here is
// a function of its arguments, which is what makes the rules testable as a
// direct restatement of https://tossandfetch.com/league-rules-policies/
//
// # Half-points
//
// The air bonus is +0.5 and the WWC Master handicap is +7.5, so scores are not
// integers. Rather than carry floating point through the database and risk
// rounding drift, every value in this package is an integer count of
// half-points. Convert to display points only at the edge, with Points.
package scoring

import "slices"

// HalfPoints is a score expressed in half-point units: 1 point is 2 HalfPoints.
type HalfPoints int

// Points converts a score to its human-readable point value.
func (h HalfPoints) Points() float64 { return float64(h) / 2 }

// Zone is where the trailing paws of the dog were at the moment of the catch.
type Zone int

const (
	// ZoneMiss is a throw the dog did not catch.
	ZoneMiss Zone = iota
	// Zone0_10 is a catch inside 10 yards, which scores nothing.
	Zone0_10
	Zone10_20
	Zone20_30
	Zone30_40
	Zone40_50
	// ZoneOut is a catch beyond 50 yards or outside the sidelines.
	ZoneOut
)

var zoneNames = map[Zone]string{
	ZoneMiss:  "miss",
	Zone0_10:  "0-10",
	Zone10_20: "10-20",
	Zone20_30: "20-30",
	Zone30_40: "30-40",
	Zone40_50: "40-50",
	ZoneOut:   "out",
}

func (z Zone) String() string {
	if n, ok := zoneNames[z]; ok {
		return n
	}
	return "unknown"
}

// Division is the handler's skill division, which determines their handicap.
type Division int

const (
	DivisionJunior Division = iota
	DivisionHandler
	DivisionMaster
	DivisionExpert
)

// Throw is one scored attempt within a round.
type Throw struct {
	Zone Zone
	// Air records a clear jumping catch with all four paws off the ground.
	Air bool
}

// Flags are the properties of a team that alter how its throws score.
type Flags struct {
	// Tiny marks a dog measuring 40cm or less at the withers.
	Tiny bool
	// Roller marks a team competing with rollers rather than flying catches.
	// It changes what counts as a catch on the field but not the arithmetic,
	// so it carries no weight here beyond disqualifying handicap points.
	Roller bool
	// Division is the handler's division.
	Division Division
}

// zoneValue is the base worth of a catch in each zone, before any bonus.
var zoneValue = map[Zone]HalfPoints{
	ZoneMiss:  0,
	Zone0_10:  0,
	Zone10_20: 2,
	Zone20_30: 4,
	Zone30_40: 6,
	Zone40_50: 10,
	ZoneOut:   0,
}

const (
	airBonus  HalfPoints = 1 // 0.5 points
	tinyBonus HalfPoints = 2 // 1 point
)

// isScoringCatch reports whether a throw is a catch that carries point value,
// and so whether there is anything for a bonus to attach to.
//
// A miss is not a catch. A catch inside 10 yards or beyond 50 is a catch, but
// the rules value it at zero and list neither the air bonus nor the tiny-dog
// bonus against it — the published tiny-dog table starts at 10-20 yards.
func isScoringCatch(t Throw) bool {
	return zoneValue[t.Zone] > 0
}

// juniorShortCatch is the consolation value a junior handler earns for a catch
// that lands short of 10 yards or out of bounds.
const juniorShortCatch HalfPoints = 2 // 1 point

// ScoreThrow returns the value of a single throw.
func ScoreThrow(t Throw, f Flags) HalfPoints {
	// A junior handler earns a flat point for a good-faith catch that falls
	// short or lands out, and explicitly no air bonus on top of it.
	if f.Division == DivisionJunior && (t.Zone == Zone0_10 || t.Zone == ZoneOut) {
		return juniorShortCatch
	}

	if !isScoringCatch(t) {
		return 0
	}

	score := zoneValue[t.Zone]
	if f.Tiny {
		score += tinyBonus
	}
	if t.Air {
		score += airBonus
	}
	return score
}

// ScoreRound returns the score of a single round.
//
// Under a format with a scored-throw cap (90:/5), only that many of the
// highest-scoring catches count. Ranking happens on each catch's full value,
// bonuses included, so an air catch cannot be displaced by a plain catch in
// the same zone.
func ScoreRound(ts []Throw, f Flags, fm Format) HalfPoints {
	values := make([]HalfPoints, 0, len(ts))
	for _, t := range ts {
		values = append(values, ScoreThrow(t, f))
	}

	if fm.ScoredThrowCap > 0 && len(values) > fm.ScoredThrowCap {
		slices.SortFunc(values, func(a, b HalfPoints) int { return int(b - a) })
		values = values[:fm.ScoredThrowCap]
	}

	var total HalfPoints
	for _, v := range values {
		total += v
	}
	return total
}

// ScoreWeek returns a team's score for one league week, given every round it
// played that week.
//
// Order matters. The tiny-dog cap clamps what was earned in play; the
// division handicap is then awarded on top, so a capped team still benefits
// from its division. Roller teams forgo the handicap entirely.
func ScoreWeek(rounds [][]Throw, f Flags, fm Format) HalfPoints {
	var played HalfPoints
	for _, r := range rounds {
		played += ScoreRound(r, f, fm)
	}

	if f.Tiny && fm.TinyWeeklyCap > 0 && played > fm.TinyWeeklyCap {
		played = fm.TinyWeeklyCap
	}

	if f.Roller {
		return played
	}
	return played + fm.Handicaps[f.Division]
}

// ScoreSeason returns a team's season total from its weekly scores, dropping
// the single lowest week.
//
// A missed week is passed in as a zero, which makes it the natural drop. Only
// one week is ever dropped, so a second miss still costs the team.
func ScoreSeason(weeks []HalfPoints) HalfPoints {
	if len(weeks) < 2 {
		var only HalfPoints
		for _, w := range weeks {
			only += w
		}
		return only
	}

	total, lowest := HalfPoints(0), weeks[0]
	for _, w := range weeks {
		total += w
		if w < lowest {
			lowest = w
		}
	}
	return total - lowest
}
