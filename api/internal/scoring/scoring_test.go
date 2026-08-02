package scoring

import "testing"

// hp converts a human-readable point value into half-points, so the tables
// below can be read against the published rules without mental arithmetic.
func hp(points float64) HalfPoints {
	return HalfPoints(points * 2)
}

// Half-points exist so the 0.5 air bonus and the 7.5 WWC handicap survive the
// database without floating point. Points is the only place they turn back
// into the numbers a human reads.
func TestHalfPoints_Points(t *testing.T) {
	tests := []struct {
		half HalfPoints
		want float64
	}{
		{0, 0},
		{1, 0.5},
		{2, 1},
		{11, 5.5},
		{15, 7.5},
		{110, 55},
	}

	for _, tc := range tests {
		if got := tc.half.Points(); got != tc.want {
			t.Errorf("HalfPoints(%d).Points() = %v, want %v", tc.half, got, tc.want)
		}
	}
}

func TestZone_String(t *testing.T) {
	tests := []struct {
		zone Zone
		want string
	}{
		{ZoneMiss, "miss"},
		{Zone0_10, "0-10"},
		{Zone10_20, "10-20"},
		{Zone20_30, "20-30"},
		{Zone30_40, "30-40"},
		{Zone40_50, "40-50"},
		{ZoneOut, "out"},
		{Zone(99), "unknown"},
	}

	for _, tc := range tests {
		if got := tc.zone.String(); got != tc.want {
			t.Errorf("Zone(%d).String() = %q, want %q", int(tc.zone), got, tc.want)
		}
	}
}

// Zone values for a standard dog, from the rules:
//
//	0-10 yards = 0 points
//	10-20 yards = 1 point
//	20-30 yards = 2 points
//	30-40 yards = 3 points
//	40-50 yards = 5 points
//	beyond 50 yards = 0 points
func TestScoreThrow_StandardDogZones(t *testing.T) {
	standard := Flags{Division: DivisionExpert}

	tests := []struct {
		name string
		zone Zone
		want HalfPoints
	}{
		{"a miss scores nothing", ZoneMiss, hp(0)},
		{"0-10 yards scores nothing", Zone0_10, hp(0)},
		{"10-20 yards scores 1", Zone10_20, hp(1)},
		{"20-30 yards scores 2", Zone20_30, hp(2)},
		{"30-40 yards scores 3", Zone30_40, hp(3)},
		{"40-50 yards scores 5", Zone40_50, hp(5)},
		{"beyond 50 yards scores nothing", ZoneOut, hp(0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreThrow(Throw{Zone: tc.zone}, standard)
			if got != tc.want {
				t.Errorf("ScoreThrow(%v) = %v half-points, want %v", tc.zone, got, tc.want)
			}
		})
	}
}

// "A 0.5-point bonus is awarded for a clear jumping catch with all four paws
// off the ground."
//
// The bonus attaches to a catch. A zone that scores nothing is not a scoring
// catch, so there is nothing for the bonus to attach to.
func TestScoreThrow_AirBonus(t *testing.T) {
	standard := Flags{Division: DivisionExpert}

	tests := []struct {
		name string
		zone Zone
		want HalfPoints
	}{
		{"air on a 1-point catch scores 1.5", Zone10_20, hp(1.5)},
		{"air on a 2-point catch scores 2.5", Zone20_30, hp(2.5)},
		{"air on a 3-point catch scores 3.5", Zone30_40, hp(3.5)},
		{"air on a 5-point catch scores 5.5", Zone40_50, hp(5.5)},
		{"air inside 10 yards still scores nothing", Zone0_10, hp(0)},
		{"air beyond 50 yards still scores nothing", ZoneOut, hp(0)},
		{"air on a miss is meaningless and scores nothing", ZoneMiss, hp(0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreThrow(Throw{Zone: tc.zone, Air: true}, standard)
			if got != tc.want {
				t.Errorf("ScoreThrow(%v, air) = %v half-points, want %v", tc.zone, got, tc.want)
			}
		})
	}
}

// "Junior Handler Exception: A good-faith throw that reaches the 0-10 yard
// zone or goes out-of-bounds earns 1 point if attempted from at least 10 yards
// away. No Air Bonus is awarded."
//
// The rules elsewhere describe this as "junior roller teams earn 1 point for
// catches short of 10 yards or out-of-bounds", so it attaches to a catch: a
// dropped disc is still nothing.
func TestScoreThrow_JuniorHandlerException(t *testing.T) {
	junior := Flags{Division: DivisionJunior}

	tests := []struct {
		name  string
		throw Throw
		want  HalfPoints
	}{
		{"a catch inside 10 yards earns 1", Throw{Zone: Zone0_10}, hp(1)},
		{"a catch out of bounds earns 1", Throw{Zone: ZoneOut}, hp(1)},
		{"no air bonus is awarded inside 10 yards", Throw{Zone: Zone0_10, Air: true}, hp(1)},
		{"no air bonus is awarded out of bounds", Throw{Zone: ZoneOut, Air: true}, hp(1)},
		{"a miss is still nothing", Throw{Zone: ZoneMiss}, hp(0)},
		{"scoring zones are unaffected", Throw{Zone: Zone30_40}, hp(3)},
		{"the air bonus still applies in a scoring zone", Throw{Zone: Zone30_40, Air: true}, hp(3.5)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreThrow(tc.throw, junior)
			if got != tc.want {
				t.Errorf("ScoreThrow(%+v, junior) = %v half-points, want %v", tc.throw, got, tc.want)
			}
		})
	}
}

// The junior exception strips the air bonus by name but says nothing about
// the tiny-dog bonus, and the rules separately confirm that junior roller
// teams and tiny roller teams each keep their own award. So the tiny bonus
// stacks on the junior consolation point.
func TestScoreThrow_JuniorHandlerWithTinyDog(t *testing.T) {
	juniorTiny := Flags{Tiny: true, Division: DivisionJunior}

	tests := []struct {
		name  string
		throw Throw
		want  HalfPoints
	}{
		{"a short catch earns the junior point plus the tiny bonus", Throw{Zone: Zone0_10}, hp(2)},
		{"an out-of-bounds catch earns both as well", Throw{Zone: ZoneOut}, hp(2)},
		{"the air bonus is still withheld", Throw{Zone: Zone0_10, Air: true}, hp(2)},
		{"a miss earns neither", Throw{Zone: ZoneMiss}, hp(0)},
		{"scoring zones are unaffected and keep the tiny bonus", Throw{Zone: Zone30_40}, hp(4)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreThrow(tc.throw, juniorTiny)
			if got != tc.want {
				t.Errorf("ScoreThrow(%+v, junior+tiny) = %v half-points, want %v", tc.throw, got, tc.want)
			}
		})
	}
}

// throws is a shorthand for building a round from zones, so the round tests
// stay readable.
func throws(zones ...Zone) []Throw {
	out := make([]Throw, len(zones))
	for i, z := range zones {
		out[i] = Throw{Zone: z}
	}
	return out
}

// "60:/All Format: 60-second rounds where all successful throws and catches
// during the round are scored."
func TestScoreRound_60All(t *testing.T) {
	standard := Flags{Division: DivisionExpert}

	got := ScoreRound(throws(Zone40_50, Zone30_40, Zone20_30, Zone10_20, Zone40_50, Zone30_40), standard, Format60All())
	want := hp(5 + 3 + 2 + 1 + 5 + 3)

	if got != want {
		t.Errorf("ScoreRound = %v half-points, want %v", got, want)
	}
}

// "90:/5 Format: 90-second rounds where only the five highest scoring catches
// per round are counted."
func TestScoreRound_90Top5(t *testing.T) {
	standard := Flags{Division: DivisionExpert}

	tests := []struct {
		name string
		ts   []Throw
		want HalfPoints
	}{
		{
			"counts only the five highest of six catches",
			throws(Zone40_50, Zone40_50, Zone30_40, Zone30_40, Zone20_30, Zone10_20),
			hp(5 + 5 + 3 + 3 + 2), // the 1-point catch is dropped
		},
		{
			"counts everything when fewer than five catches were made",
			throws(Zone40_50, Zone10_20),
			hp(5 + 1),
		},
		{
			"misses never displace a scoring catch",
			throws(ZoneMiss, ZoneMiss, Zone10_20, ZoneMiss, Zone20_30, ZoneMiss),
			hp(1 + 2),
		},
		{
			"keeps the highest five regardless of the order they were thrown",
			throws(Zone10_20, Zone40_50, Zone10_20, Zone30_40, Zone10_20, Zone20_30, Zone10_20),
			hp(5 + 3 + 2 + 1 + 1),
		},
		{
			"an empty round scores nothing",
			nil,
			hp(0),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreRound(tc.ts, standard, Format90Top5())
			if got != tc.want {
				t.Errorf("ScoreRound = %v half-points, want %v", got, tc.want)
			}
		})
	}
}

// The air bonus is part of a catch's value, so it must be applied before the
// top-five selection rather than after — otherwise a 3+air catch could lose to
// a plain 3 and change the total.
func TestScoreRound_90Top5_RanksByValueIncludingBonuses(t *testing.T) {
	standard := Flags{Division: DivisionExpert}

	ts := []Throw{
		{Zone: Zone30_40, Air: true}, // 3.5
		{Zone: Zone30_40},            // 3
		{Zone: Zone30_40},            // 3
		{Zone: Zone30_40},            // 3
		{Zone: Zone30_40},            // 3
		{Zone: Zone20_30},            // 2 - dropped
	}

	got := ScoreRound(ts, standard, Format90Top5())
	want := hp(3.5 + 3 + 3 + 3 + 3)

	if got != want {
		t.Errorf("ScoreRound = %v half-points, want %v", got, want)
	}
}

// A round worth exactly n points for a standard dog, built from 1-point
// catches, so week-level tests can state intent without arithmetic.
func roundWorth(n int) []Throw {
	ts := make([]Throw, n)
	for i := range ts {
		ts[i] = Throw{Zone: Zone10_20}
	}
	return ts
}

// "Divisions & Handicap Points: Junior +10/week, Handler +10/week,
// Master +5/week, Expert 0/week."
//
// Per week — not per round. A two-round week adds the handicap once.
func TestScoreWeek_HandicapAppliedOncePerWeek(t *testing.T) {
	week := [][]Throw{roundWorth(8), roundWorth(6)}

	tests := []struct {
		name     string
		division Division
		want     HalfPoints
	}{
		{"an expert gets no handicap", DivisionExpert, hp(14)},
		{"a master gets 5 once, not 5 per round", DivisionMaster, hp(14 + 5)},
		{"a handler gets 10 once", DivisionHandler, hp(14 + 10)},
		{"a junior gets 10 once", DivisionJunior, hp(14 + 10)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreWeek(week, Flags{Division: tc.division}, Format60All())
			if got != tc.want {
				t.Errorf("ScoreWeek = %v half-points, want %v", got, tc.want)
			}
		})
	}
}

// "No handicap points are awarded to Roller Teams."
func TestScoreWeek_RollerTeamsGetNoHandicap(t *testing.T) {
	week := [][]Throw{roundWorth(8), roundWorth(6)}

	got := ScoreWeek(week, Flags{Division: DivisionHandler, Roller: true}, Format60All())
	want := hp(14)

	if got != want {
		t.Errorf("ScoreWeek(roller handler) = %v half-points, want %v", got, want)
	}
}

// "A Tiny Dog Team's combined 2-round weekly score is capped at 55 points."
//
// The cap is on the score earned in play. The handicap is awarded on top of
// it, so a capped team still benefits from its division.
func TestScoreWeek_TinyDogCap(t *testing.T) {
	tests := []struct {
		name     string
		rounds   [][]Throw
		division Division
		want     HalfPoints
	}{
		{
			// 10 catches at 1 point plus 10 tiny bonuses = 20 per round.
			"a weekly total under the cap is untouched",
			[][]Throw{roundWorth(10), roundWorth(10)},
			DivisionExpert,
			hp(40),
		},
		{
			"a weekly total over the cap is clamped to 55",
			[][]Throw{roundWorth(30), roundWorth(30)},
			DivisionExpert,
			hp(55),
		},
		{
			"the handicap is added after the cap, not before",
			[][]Throw{roundWorth(30), roundWorth(30)},
			DivisionMaster,
			hp(55 + 5),
		},
		{
			"a modest week stays below the cap and keeps its tiny bonuses",
			[][]Throw{roundWorth(5), roundWorth(5)}, // 10 play + 10 tiny bonus
			DivisionExpert,
			hp(20),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreWeek(tc.rounds, Flags{Tiny: true, Division: tc.division}, Format60All())
			if got != tc.want {
				t.Errorf("ScoreWeek = %v half-points, want %v", got, tc.want)
			}
		})
	}
}

// The cap belongs to tiny dogs only; a standard dog can score above 55.
func TestScoreWeek_StandardDogIsNotCapped(t *testing.T) {
	week := [][]Throw{roundWorth(40), roundWorth(40)}

	got := ScoreWeek(week, Flags{Division: DivisionExpert}, Format60All())
	want := hp(80)

	if got != want {
		t.Errorf("ScoreWeek(standard dog) = %v half-points, want %v", got, want)
	}
}

// "Each Team's lowest-scoring week is dropped at season's end. If a Team
// misses a week, that becomes the dropped score."
func TestScoreSeason_MulliganDropsLowestWeek(t *testing.T) {
	tests := []struct {
		name  string
		weeks []HalfPoints
		want  HalfPoints
	}{
		{
			"the lowest of five weeks is dropped",
			[]HalfPoints{hp(30), hp(25), hp(12), hp(28), hp(31)},
			hp(30 + 25 + 28 + 31),
		},
		{
			"a missed week scores zero and becomes the drop",
			[]HalfPoints{hp(30), hp(25), hp(0), hp(28), hp(31)},
			hp(30 + 25 + 28 + 31),
		},
		{
			"only one week is ever dropped, so a second miss still costs",
			[]HalfPoints{hp(30), hp(0), hp(0), hp(28), hp(31)},
			hp(30 + 0 + 28 + 31),
		},
		{
			"ties drop only one of the tied weeks",
			[]HalfPoints{hp(10), hp(10), hp(10)},
			hp(20),
		},
		{
			"a single week has nothing to drop against",
			[]HalfPoints{hp(30)},
			hp(30),
		},
		{
			"an empty season scores nothing",
			nil,
			hp(0),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreSeason(tc.weeks)
			if got != tc.want {
				t.Errorf("ScoreSeason(%v) = %v half-points, want %v", tc.weeks, got, tc.want)
			}
		})
	}
}

// The WWC uses larger handicaps, and the Master's +7.5 must stay exact.
func TestScoreWeek_WWCHandicaps(t *testing.T) {
	week := [][]Throw{roundWorth(10), roundWorth(10), roundWorth(10)}

	tests := []struct {
		name     string
		division Division
		want     HalfPoints
	}{
		{"expert gets nothing", DivisionExpert, hp(30)},
		{"master gets exactly 7.5", DivisionMaster, hp(37.5)},
		{"handler gets 15", DivisionHandler, hp(45)},
		{"junior gets 15", DivisionJunior, hp(45)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreWeek(week, Flags{Division: tc.division}, FormatWWC())
			if got != tc.want {
				t.Errorf("ScoreWeek(WWC) = %v half-points, want %v", got, tc.want)
			}
		})
	}
}

// "Tiny Dogs receive 1 bonus point per catch in addition to standard scoring."
//
// Per *catch* — a missed throw is not a catch, and the rules list the tiny
// values only for the scoring zones (10-20 = 2, 20-30 = 3, 30-40 = 4,
// 40-50 = 6).
func TestScoreThrow_TinyDogBonus(t *testing.T) {
	tiny := Flags{Tiny: true, Division: DivisionExpert}

	tests := []struct {
		name  string
		throw Throw
		want  HalfPoints
	}{
		{"10-20 yards scores 2", Throw{Zone: Zone10_20}, hp(2)},
		{"20-30 yards scores 3", Throw{Zone: Zone20_30}, hp(3)},
		{"30-40 yards scores 4", Throw{Zone: Zone30_40}, hp(4)},
		{"40-50 yards scores 6", Throw{Zone: Zone40_50}, hp(6)},
		{"air bonus still applies on top", Throw{Zone: Zone40_50, Air: true}, hp(6.5)},
		{"a miss is not a catch and earns no bonus", Throw{Zone: ZoneMiss}, hp(0)},
		{"a catch inside 10 yards is not a scoring catch", Throw{Zone: Zone0_10}, hp(0)},
		{"a catch beyond 50 yards is not a scoring catch", Throw{Zone: ZoneOut}, hp(0)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScoreThrow(tc.throw, tiny)
			if got != tc.want {
				t.Errorf("ScoreThrow(%+v, tiny) = %v half-points, want %v", tc.throw, got, tc.want)
			}
		})
	}
}
