# Fetch and Score

Mobile-first scoring for [K9 Frisbee Toss & Fetch](https://tossandfetch.com/) leagues.

Run a league week from your phone at the field: open the day's session, run the
official round timer with spoken cues, tap throws in as they happen, and let
everyone watching see the score update live.

## Layout

| Path | What |
|---|---|
| `api/` | Go backend — JSON API, SQLite, SSE |
| `api/internal/scoring/` | The rules engine. Pure, stdlib-only, heavily tested. |
| `web/` | Static site — plain HTML + Alpine.js + Tailwind, deployed to GitHub Pages |
| `e2e/` | Playwright end-to-end tests |
| `deploy/` | Dockerfile, compose, Cloudflare Tunnel config |
| `docs/superpowers/specs/` | Design docs |

The frontend is a static site on GitHub Pages at `fetchandscore.com`. The
backend is a single container talking to one SQLite file, hosted separately.

## Points are stored as half-points

The air bonus is +0.5 and the WWC Master handicap is +7.5, so every score in
this codebase is an **integer count of half-points** (`total_x2`). Nothing
touches floating point. Divide by two only at the moment of display.

## Development

```sh
make dev      # tailwind watch + api on a local sqlite file + static server
make seed     # load a demo club, season and session
make test     # go tests + js tests
make check    # everything CI runs, locally
```

See `docs/superpowers/specs/` for the design and the scope of the current slice.

## Rules

Scoring follows the published
[league rules](https://tossandfetch.com/league-rules-policies/). The
authoritative implementation is `api/internal/scoring/`, and its test suite is
written to be readable as a restatement of those rules.

## License

MIT
