package scoring

// Format is everything about a season that varies between the league's play
// formats. It is stored as data on the season rather than branched on in code,
// so a new variant (the WWC's three rounds, or a club running a two-minute
// round) is a row rather than a release.
type Format struct {
	Name string

	// RoundSeconds is the length of one round.
	RoundSeconds int
	// RoundsPerWeek is how many rounds each team plays per league week.
	RoundsPerWeek int

	// ScoredThrowCap is how many of a round's highest-scoring catches count.
	// Zero means every catch counts.
	ScoredThrowCap int

	// TinyWeeklyCap clamps a tiny dog's combined weekly score. Zero means no
	// cap. It applies only to tiny dogs, and only to the weekly total.
	TinyWeeklyCap HalfPoints

	// Handicaps are added once per week, by division.
	Handicaps map[Division]HalfPoints

	// CueSeconds are the remaining-time marks at which the timer speaks, in
	// descending order. The league's standard calls are 30, 10, then a
	// 5-4-3-2-1 countdown to TIME.
	CueSeconds []int
}

// standardHandicaps are the weekly league handicap points by division:
// Junior +10, Handler +10, Master +5, Expert 0.
func standardHandicaps() map[Division]HalfPoints {
	return map[Division]HalfPoints{
		DivisionJunior:  20,
		DivisionHandler: 20,
		DivisionMaster:  10,
		DivisionExpert:  0,
	}
}

// tinyWeeklyCap is the 55-point ceiling on a tiny dog team's combined
// two-round weekly score.
const tinyWeeklyCap HalfPoints = 110

// Format60All is the 60-second format in which every successful catch scores.
func Format60All() Format {
	return Format{
		Name:           "60:/All",
		RoundSeconds:   60,
		RoundsPerWeek:  2,
		ScoredThrowCap: 0,
		TinyWeeklyCap:  tinyWeeklyCap,
		Handicaps:      standardHandicaps(),
		CueSeconds:     []int{30, 10, 5, 4, 3, 2, 1},
	}
}

// Format90Top5 is the 90-second format in which only the five highest-scoring
// catches per round count.
func Format90Top5() Format {
	return Format{
		Name:           "90:/5",
		RoundSeconds:   90,
		RoundsPerWeek:  2,
		ScoredThrowCap: 5,
		TinyWeeklyCap:  tinyWeeklyCap,
		Handicaps:      standardHandicaps(),
		CueSeconds:     []int{60, 30, 10, 5, 4, 3, 2, 1},
	}
}

// FormatWWC is the Worldwide Championship format: three 60-second rounds, all
// catches scored, and a larger handicap.
//
// The 55-point tiny-dog cap is defined against a "combined 2-round weekly
// score" and so does not carry over to a three-round championship.
func FormatWWC() Format {
	return Format{
		Name:           "WWC",
		RoundSeconds:   60,
		RoundsPerWeek:  3,
		ScoredThrowCap: 0,
		TinyWeeklyCap:  0,
		Handicaps: map[Division]HalfPoints{
			DivisionJunior:  30,
			DivisionHandler: 30,
			DivisionMaster:  15,
			DivisionExpert:  0,
		},
		CueSeconds: []int{30, 10, 5, 4, 3, 2, 1},
	}
}
