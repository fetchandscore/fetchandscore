// Command fnsctl is the admin tool for Fetch and Score.
//
// Slice one has no management UI, so clubs, seasons and sessions are created
// here. It talks to the database directly, so it runs wherever the file is.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fetchandscore/fetchandscore/api/internal/auth"
	"github.com/fetchandscore/fetchandscore/api/internal/mail"
	"github.com/fetchandscore/fetchandscore/api/internal/scoring"
	"github.com/fetchandscore/fetchandscore/api/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "migrate":
		return cmdMigrate(args[1:])
	case "seed":
		return cmdSeed(args[1:])
	case "invite":
		return cmdInvite(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fnsctl - Fetch and Score admin

Usage:
  fnsctl migrate                            apply pending schema migrations
  fnsctl seed    [-db path]                 create a demo club, season and session
  fnsctl invite  -club slug -email addr     invite someone to a club

Every command accepts -db (default $FNS_DB_PATH, else data/fetchandscore.db).
`)
}

// openStore opens the database, applying migrations on the way.
func openStore(fs *flag.FlagSet, args []string) (*store.Store, error) {
	def := os.Getenv("FNS_DB_PATH")
	if def == "" {
		def = "data/fetchandscore.db"
	}
	dbPath := fs.String("db", def, "path to the SQLite database")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return store.Open(*dbPath)
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	// Opening applies migrations; reaching here means the schema is current.
	fmt.Println("schema is up to date")
	return nil
}

func cmdInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	clubSlug := fs.String("club", "", "club slug")
	email := fs.String("email", "", "address to invite")
	role := fs.String("role", "member", "member, scorekeeper, captain or admin")
	baseURL := fs.String("base-url", env("FNS_BASE_URL", "https://fetchandscore.com"), "public site URL")

	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if *clubSlug == "" || *email == "" {
		return errors.New("both -club and -email are required")
	}

	ctx := context.Background()
	var clubID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM clubs WHERE slug = ?`, *clubSlug).Scan(&clubID); err != nil {
		return fmt.Errorf("no club with slug %q: %w", *clubSlug, err)
	}

	svc := auth.New(st, &mail.Recorder{}, *baseURL)
	link, err := svc.Invite(ctx, clubID, *email, store.Role(*role), nil)
	if err != nil {
		return err
	}

	fmt.Printf("Invited %s to %s as %s.\n\nSend them this link:\n  %s\n",
		*email, *clubSlug, *role, link)
	fmt.Printf("\nThey can also just request a sign-in link at %s once invited.\n", *baseURL)
	return nil
}

// cmdSeed builds a complete, playable demo: a club, a 60:/All season, three
// teams with different designations, and tonight's session with two rounds
// each. Enough to exercise every scoring rule by hand.
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	club, err := st.CreateClub(ctx, "demo", "Demo Disc Dogs")
	if err != nil {
		return fmt.Errorf("creating club (already seeded?): %w", err)
	}

	season, err := st.CreateSeason(ctx, store.NewSeason{
		ClubID:    club.ID,
		Name:      "Summer",
		Year:      time.Now().Year(),
		FormatKey: "60_all",
		Format:    scoring.Format60All(),
		WeekCount: 5,
		StartsOn:  time.Now().Format("2006-01-02"),
	})
	if err != nil {
		return err
	}

	session, err := st.CreatePlaySession(ctx, store.NewPlaySession{
		ClubID:   club.ID,
		Name:     "Week 1",
		StartsAt: time.Now(),
	})
	if err != nil {
		return err
	}

	demoTeams := []struct {
		email    string
		person   string
		dog      string
		tiny     bool
		roller   bool
		division scoring.Division
	}{
		{"brad@example.com", "Brad", "Sadie", false, false, scoring.DivisionExpert},
		{"alex@example.com", "Alex", "Pip", true, false, scoring.DivisionHandler},
		{"sam@example.com", "Sam", "Moose", false, true, scoring.DivisionMaster},
	}

	for i, d := range demoTeams {
		user, err := st.CreateUser(ctx, d.email, d.person)
		if err != nil {
			return err
		}
		role := store.RoleScorekeeper
		if i == 0 {
			role = store.RoleCaptain
		}
		if err := st.AddClubMember(ctx, club.ID, user.ID, role); err != nil {
			return err
		}

		dog, err := st.CreateDog(ctx, user.ID, d.dog, nil, d.tiny)
		if err != nil {
			return err
		}
		team, err := st.CreateTeam(ctx, club.ID, user.ID, dog.ID, d.dog+" · "+d.person)
		if err != nil {
			return err
		}
		entry, err := st.CreateSeasonEntry(ctx, store.NewSeasonEntry{
			SeasonID: season.ID, TeamID: team.ID,
			Division: d.division, Roller: d.roller, Tiny: d.tiny,
		})
		if err != nil {
			return err
		}
		if err := st.AddSessionTeam(ctx, session.ID, entry.ID, i+1); err != nil {
			return err
		}
		for round := 1; round <= season.Format.RoundsPerWeek; round++ {
			if _, err := st.CreateRound(ctx, session.ID, entry.ID, round); err != nil {
				return err
			}
		}
	}

	fmt.Printf(`Seeded.

  Club:    %s (slug: %s)
  Season:  %s %d, %s
  Session: %s (id %d) with %d teams, %d rounds each

Sign in as brad@example.com (captain). In development the sign-in link is
printed to the server log rather than emailed.
`,
		club.Name, club.Slug,
		season.Name, season.Year, season.Format.Name,
		session.Name, session.ID, len(demoTeams), season.Format.RoundsPerWeek)
	return nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
