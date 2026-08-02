# Fetch and Score — Rebuild Design & Implementation Plan

## Context

`fetchandscore.com` is today a Jekyll site with a hardcoded, non-persisting score form (2 rounds × 5 throws of radio buttons), static YAML for seasons and teams, a dangling AWS Cognito login link, and an unused `aws-sdk.min.js`. Nothing is stored, nothing is shared, and it does not implement the actual K9 Frisbee Toss & Fetch rules. The `backend` repo is empty.

The goal is a mobile-first scoring app that a club can actually run a league week on: sign in, open the day's session, run the official round timer with spoken cues, tap throws in as they happen, and have everyone at the field watch the score update live. Scores must be computed by the real published rules, not approximated.

This plan covers **slice 1 only** — scoring one round end-to-end. Season standings, handicap application, the mulligan drop, promotion tracking and admin UIs are deliberately out of scope for slice 1, though the schema and scoring engine account for them from day one so they are additive later.

## Decisions (agreed)

| Area | Decision |
|---|---|
| Repo | Single monorepo at `fetchandscore/fetchandscore` (renamed from `fetchandscore.github.io`; `fetchandscore/backend` archived). Pages deploys from this same repo via Actions — no cross-repo publishing. |
| Backend | Go, `net/http` stdlib routing, SQLite via `modernc.org/sqlite` (pure Go → static binary). |
| Frontend | Plain HTML + Alpine.js + JSON API + SSE. No SSG, no Jekyll, no Ruby. |
| CSS | Tailwind via the standalone CLI binary — no Node, no `node_modules` for the build. |
| Auth | Invite-only. Email magic link via the Mailgun API, HttpOnly session cookies. |
| Realtime | Server-Sent Events. |
| Offline | Online-first with a client-side write queue that survives dropouts. Not full offline-first. |
| Timer audio | Pre-generated TTS clips scheduled on the WebAudio clock. Includes a "ready, set, GO" 3-second pre-roll. |
| Throw entry | Single-tap grid: every zone × air combination is its own button. |
| Org model | Mirrors official structure: Club → Season → Week → Session → Round; Team = handler + dog. |
| Visibility | Live sessions require club membership; finished results are public. |
| Look | High-contrast, bold, sunlight-legible. Light default, dark theme available. |
| PWA | Manifest + icons + Screen Wake Lock + a minimal app-shell service worker. |
| Hosting | Home server + Cloudflare Tunnel initially. Portable to any VPS (Hostinger etc.). |
| CI | Self-contained GitHub Actions **plus** SonarCloud. |

**Explicitly rejected:** AWS Lambda. It is incompatible with both SQLite (needs a persistent disk) and SSE (needs a long-lived connection). Moving to a VPS is a drop-in; Lambda would mean replacing the database *and* the realtime layer.

## Rules being implemented

Source: <https://tossandfetch.com/league-rules-policies/>

- **Zones:** 0–10 = 0, 10–20 = 1, 20–30 = 2, 30–40 = 3, 40–50 = 5, beyond 50 = 0. Trailing paws decide; on a line, the higher value.
- **Air bonus:** +0.5 for a clear four-paws-off catch.
- **Formats:** `60:/All` (60s, every catch scores) and `90:/5` (90s, only the five highest catches score).
- **Tiny Dog** (≤40cm): +1 per *catch*; combined 2-round weekly score capped at 55.
- **Junior handler:** a good-faith throw reaching 0–10 or out-of-bounds from ≥10 yards scores 1; no air bonus.
- **Roller:** season-locked designation, no handicap, otherwise scored normally.
- **Handicaps (per week, not per round):** Junior +10, Handler +10, Master +5, Expert 0. WWC: +15 / +7.5 / 0.
- **Season:** 5 weeks, 2 rounds per week, lowest week dropped; a missed week is the dropped week.
- **Timer calls:** 30s, 10s, then 5-4-3-2-1-TIME. A throw released before the "T" in TIME is in play.

Because of the 0.5 air bonus and the 7.5 WWC handicap, **all points are stored as integer half-points** (`points_x2`). No floating-point money-style bugs.

### Rule interpretations

Two clauses in the published rules are genuinely ambiguous. Implementing them
required picking a reading; both are decided below, encoded in
`api/internal/scoring/`, and each is a single constant should the league rule
otherwise.

**A bonus needs a scoring catch to attach to.** A catch inside 10 yards or out
of bounds scores zero, and neither the air bonus nor the tiny-dog bonus applies
to it. The rules say the tiny bonus is "1 bonus point per catch", but the
published tiny-dog table starts at 10–20 yards, and the air bonus is plainly
not intended to turn a zero-point catch into half a point. *Worth confirming
with a league official before the first season is scored for real.*

**The tiny bonus stacks on the junior consolation point.** A junior handler
with a tiny dog scores 2 for a catch short of 10 yards or out of bounds: 1 for
the junior good-faith catch, 1 for the tiny dog. The junior clause withholds
the air bonus *by name* and says nothing about the tiny bonus, and the rules
separately confirm that junior roller teams and tiny roller teams each keep
their own award.

**Throw distance is the scorekeeper's judgment.** The junior clause qualifies
its point with "if attempted from at least 10 yards away". The app has no way
to measure that, so recording the outcome *is* the judgment call — there is no
separate confirmation step.

## Repository layout

```
fetchandscore/
├── api/                    Go module
│   ├── cmd/fetchandscore/  main
│   ├── cmd/fnsctl/         admin CLI (seed clubs, seasons, invites)
│   ├── internal/scoring/   pure rules engine — no DB, no HTTP
│   ├── internal/store/     SQLite access + embedded migrations
│   ├── internal/http/      handlers, middleware, SSE hub
│   ├── internal/auth/      magic link, sessions, invites
│   └── internal/mail/      Mailgun client (plain net/http)
├── web/                    static site
│   ├── index.html          dashboard
│   ├── session.html        running order for a session
│   ├── round.html          timer + throw entry
│   ├── results.html        public results
│   ├── auth.html           request / verify magic link
│   ├── js/                 api.js, queue.js, timer.js, stream.js, scoring-table.js
│   ├── audio/              cue sprite (.webm/opus + .m4a/aac) + offsets.json
│   ├── css/app.css         Tailwind source
│   ├── manifest.webmanifest, sw.js, CNAME
├── e2e/                    Playwright
├── deploy/                 Dockerfile, compose.yml, cloudflared config
├── docs/superpowers/specs/ design doc (committed in step 1)
├── .github/workflows/
└── Makefile
```

## Backend design

**Dependencies: one.** `modernc.org/sqlite`. Routing uses Go 1.22+ stdlib patterns (`mux.HandleFunc("POST /api/rounds/{id}/throws", …)`). Mailgun is a form-POST to their API over `net/http` — no SDK. Migrations are `.sql` files via `embed` applied in order against a `schema_migrations` table (~60 lines, no library).

### Schema (slice 1)

```
users(id, email UNIQUE, name, created_at)
clubs(id, slug UNIQUE, name)
club_members(club_id, user_id, role)        role: member|scorekeeper|captain|admin
invites(id, club_id, email, role, token_hash, expires_at, accepted_at)
sessions_auth(id, user_id, token_hash, expires_at, last_seen_at, user_agent)
dogs(id, owner_user_id, name, height_cm, tiny)
teams(id, club_id, handler_user_id, dog_id, display_name)
seasons(id, club_id, name, year, format, round_seconds, scored_throw_cap,
        rounds_per_week, weekly_cap_x2, starts_on, week_count)
season_entries(id, season_id, team_id, division, roller, tiny)
weeks(id, season_id, idx, scheduled_for)
play_sessions(id, club_id, week_id NULL, starts_at, status)
play_session_teams(play_session_id, season_entry_id, running_order)
rounds(id, play_session_id, season_entry_id, round_number, status,
       started_at, ended_at, confirmed_at, scorekeeper_user_id, total_x2)
throws(id, round_id, seq, zone, air, client_id UNIQUE, void, recorded_at)
audit_log(id, actor_user_id, entity, entity_id, action, before, after, at)
```

`format`, `round_seconds`, `scored_throw_cap` and `weekly_cap_x2` live on the season as **data**, so the WWC 3-round variant or a 2-minute custom format is a row, not a code change.

### `internal/scoring` — the crown jewels

A pure package with no imports beyond stdlib. This is where correctness lives and where test effort concentrates.

```go
type Zone int          // ZoneMiss, Zone0_10, Zone10_20, Zone20_30, Zone30_40, Zone40_50
type HalfPoints int    // always ×2

type Throw  struct{ Zone Zone; Air bool }
type Flags  struct{ Tiny, Roller, Junior bool; Division Division }
type Format struct{ RoundSeconds, ScoredThrowCap int; WeeklyCapX2 int; Handicaps map[Division]HalfPoints }

func ScoreThrow(t Throw, f Flags) HalfPoints
func ScoreRound(ts []Throw, f Flags, fm Format) HalfPoints   // applies top-N cap
func ScoreWeek(rounds [][]Throw, f Flags, fm Format) HalfPoints // tiny cap, then handicap
func ScoreSeason(weeks []HalfPoints, fm Format) HalfPoints   // mulligan drop
```

`ScoreWeek`/`ScoreSeason` are implemented and tested in slice 1 even though no UI surfaces them yet — they are cheap, and retrofitting them against stored data later is not.

**Test cases that must exist:** air bonus never applies to a 0-point zone; tiny bonus applies per catch and never to a miss; junior good-faith throw scores 1 with no air; 90:/5 selects the five highest *scoring* catches; the tiny 55-point weekly cap clamps after both rounds are summed but before the handicap; handicap applies once per week; the mulligan drops a missed week; WWC's 7.5 stays exact in half-points.

### API surface (slice 1)

```
POST   /api/auth/request            {email}  → sends magic link, always 202 (no enumeration)
POST   /api/auth/verify             {token}  → sets session cookie
POST   /api/auth/logout
GET    /api/me
POST   /api/clubs/{id}/invites      captain+ only
GET    /api/dashboard               active / upcoming / past sessions for the user
GET    /api/sessions/{id}           running order, teams, round statuses
POST   /api/rounds/{id}/start       → ready→running, stamps started_at
POST   /api/rounds/{id}/reset       false start
POST   /api/rounds/{id}/throws      {client_id, zone, air}  idempotent upsert
POST   /api/rounds/{id}/throws/{tid}/void
POST   /api/rounds/{id}/confirm     → locks the round
GET    /api/sessions/{id}/stream    SSE (club members only)
GET    /api/public/sessions/{id}    confirmed results, no auth
```

### Auth flow

Magic-link tokens are 32 random bytes, stored **hashed**, single-use, 15-minute expiry. The emailed link is a `GET` to a static page that then issues a `POST` to verify — so email security scanners that pre-fetch links cannot silently burn the token. Sessions are HttpOnly + Secure + SameSite=Lax cookies holding a hashed random token. Writes additionally check the `Origin` header. Auth endpoints are rate-limited per IP and per email.

### Realtime

In-process SSE hub: `map[playSessionID]map[*subscriber]struct{}`, guarded by a mutex, with a per-session ring buffer of recent events for `Last-Event-ID` replay after a reconnect. A comment heartbeat every 20 seconds keeps Cloudflare Tunnel from idling the connection. Events: `throw.added`, `throw.voided`, `round.started`, `round.confirmed`. The round's start timestamp is included so spectators can render an approximate clock.

## Frontend design

### Write queue (the "resilient buffering")

Every throw POST carries a client-generated `client_id`; the server upserts on it, so a retry after an ambiguous timeout can never double-score. The queue lives in `localStorage` keyed by round and is drained by a single in-flight worker. The UI shows a quiet pending count that escalates to a visible warning after a sustained outage. Undo voids a throw through the same queue — never a local-only delete. On reconnect: drain in order, then re-fetch round state as the source of truth.

### Timer

State machine: `ready → [START] → preroll (ready/set/GO) → running → grace → [CONFIRM] → confirmed`.

- The clock is derived from a `performance.now()` delta against the start timestamp, never accumulated from `setInterval` ticks.
- On the START gesture, the audio sprite is decoded once into an `AudioBuffer`; then **every cue for the entire round is scheduled up front** via `source.start(ctx.currentTime + offset)`. This is sample-accurate and immune to JS jank — which is precisely why live `speechSynthesis` was rejected for the 5-4-3-2-1. Reset cancels all scheduled sources.
- Cue points come from the season's format: 60s → 30, 10, 5-4-3-2-1, TIME; 90s → 60, 30, 10, 5-4-3-2-1, TIME. A custom 2-minute format can specify 30/15/5-1.
- `grace` keeps the throw buttons live after 0:00, because the rules count a throw released before the "T" in TIME. CONFIRM locks the round.
- Mute toggle persisted per device; spectator devices default to muted so the field doesn't get a chorus of phones.
- Screen Wake Lock is acquired on START and released on CONFIRM.

Audio clips are generated by a `make audio` target (Piper or espeak-ng), concatenated into one sprite plus a JSON offset map, and committed. Encoded as both `.webm`/Opus and `.m4a`/AAC, selected via `canPlayType`. ~15 clips, well under 50KB.

### Throw entry screen

Single-tap grid — every outcome is its own button, so there is no mode to leave armed:

```
┌─────────────────────────────┐
│ ROUND 1   ● 0:47      [STOP]│
│ Sadie · Brad         12.5   │
├──────────────┬──────────────┤
│  40-50 · 5   │ 40-50+AIR 5.5│
│  30-40 · 3   │ 30-40+AIR 3.5│
│  20-30 · 2   │ 20-30+AIR 2.5│
│  10-20 · 1   │ 10-20+AIR 1.5│
├──────────────┴──────────────┤
│       0-10 / OB  →  0       │
│          ✗  M I S S         │
├─────────────────────────────┤
│ 3 · 2.5 · 5 · X · 2  ↶ UNDO │
└─────────────────────────────┘
```

Minimum 64px tap targets, high-contrast zone colors chosen to stay distinguishable for the common colorblindness types and in direct sun.

**Avoiding a duplicated rules engine:** the JS needs point values for optimistic display. Rather than hand-copying them, `go generate` emits `web/js/scoring-table.js` from the Go ruleset. One source of truth, and the duplication scanner stays quiet.

## CI / quality

`.github/workflows/ci.yml` on every PR and push:

- **Go** — `go vet`, `golangci-lint` (bundles staticcheck/errcheck/etc.), `go test -race -coverprofile`, `gosec`, `govulncheck`
- **Web** — `biome ci` (JS lint + format), `html-validate`, Tailwind build check
- **Cross-cutting** — `jscpd` (copy-paste), `semgrep` OSS rules, CodeQL (go + javascript), Trivy against both the filesystem and the built image
- **E2E** — Playwright against a locally-run API + static server
- **SonarCloud** — coverage upload + quality gate blocking merge

`deploy-pages.yml` on push to `main`: build `/web` with the Tailwind binary, copy `web/CNAME` into the artifact, `upload-pages-artifact` → `deploy-pages`.

`release-api.yml` on tag: multi-arch image → `ghcr.io`, cosign signature, syft SBOM.

Dependabot for gomod, github-actions, docker and the small npm dev-tool set.

Locally, `make check` runs the identical suite (scanners via Docker) and `make dev` runs Tailwind in watch mode, the API against a local SQLite file, and a static server for `/web`. `make seed` loads a demo club, season and session.

## Testing strategy

| Layer | Approach |
|---|---|
| Rules | Table-driven Go tests over every published rule and edge case above. Highest coverage bar in the repo. |
| API | `httptest` against a temporary SQLite DB — full flows including invite → magic link → score → confirm. |
| Idempotency | Same `client_id` posted twice yields one throw; concurrent posts don't duplicate. |
| JS | `node:test` (built in, no dependency) over the write queue and timer scheduling under a fake clock. |
| E2E | Playwright: sign in via a test-only token endpoint, run a full round, assert the score. Then kill the network mid-round, keep tapping, restore, and assert the queue drains to the correct total. |

## Deployment

Multi-stage Dockerfile: build with `CGO_ENABLED=0`, ship on `distroless/static:nonroot` (~15–20MB). `deploy/compose.yml` runs the API plus a `cloudflared` sidecar, with SQLite on a bind-mounted volume. SQLite pragmas: WAL, `busy_timeout=5000`, `foreign_keys=on`, `synchronous=NORMAL`. Config entirely via environment variables (DB path, Mailgun key/domain, base URL, CORS origin). `/healthz` and `/readyz` endpoints. Backups via `VACUUM INTO` on a cron, with Litestream noted as the upgrade path for continuous replication.

## Implementation order

1. Rename the repo, scaffold the monorepo, commit the design doc to `docs/superpowers/specs/`, archive `backend`.
2. `internal/scoring` with its full test suite. **Nothing else starts until the rules are provably right.**
3. Store layer: migrations, schema, `fnsctl` seed command.
4. Auth: invites, Mailgun, magic link, sessions, rate limiting.
5. Session/round/throw API with idempotent writes, plus the SSE hub.
6. Web shell: Tailwind setup, dashboard, session view.
7. The round screen: timer, audio sprite generation, throw grid, write queue, wake lock.
8. Public results page + PWA manifest and service worker.
9. CI workflows, SonarCloud, Dependabot; Pages deploy.
10. Dockerfile, compose, Cloudflare Tunnel, backups.
11. Playwright E2E including the network-drop scenario.

Steps 2–5 and 6–8 each land as reviewable increments; 9–11 can proceed in parallel with 6–8.

## Verification

- `make check` green: all linters, scanners, Go tests with `-race`, JS tests, SonarCloud gate.
- `make dev && make seed`, then in a browser: request a magic link (dev mode prints it to the log), open the seeded session, run a round with the timer, confirm the audio fires at 30/10/5-4-3-2-1/TIME and the pre-roll plays, tap throws, hit CONFIRM, and check the total against a hand-computed score.
- Open the same session in a second browser as a different club member and confirm throws appear live via SSE.
- With DevTools set to offline, tap several throws, confirm the pending indicator appears, go back online, and confirm the total reconciles exactly with no duplicates.
- Verify on a real phone outdoors: legibility in sun, tap accuracy, home-screen install, and that the screen does not sleep during a round.
- Confirm `fetchandscore.com` still resolves and serves over HTTPS after the repo rename.

## Open items to settle during implementation

- Exact TTS voice for the cue clips (a listening test on a phone at the field beats any spec).
- Mailgun sending domain and DNS records (SPF/DKIM) for `fetchandscore.com`.
- Whether the pre-roll counts down from 3 seconds silently with only "ready, set, GO", or speaks "3, 2, 1" as well.


## Built and shipped

Slice one is complete and live at `fetchandscore.com`. What differed from the
plan above, and why:

**Alpine cannot hold objects that use private class fields.** It wraps
component state in a reactive Proxy, and a `#private` field access through a
Proxy throws. The timer, write queue, audio and screen lock live in the
component's closure instead — which is also the more honest arrangement, since
none of them are view state.

**Tailwind v4 emits only the theme variables it can see written out in the
source.** The scoring grid built its colour name dynamically, so two of the
five zones had their variables tree-shaken away and rendered as blank buttons.
Zone colours are hand-written classes now.

**The Secure flag on the session cookie derives from the base URL's scheme**
rather than from the development flag, so an HTTPS deployment cannot ship
insecure cookies even if `FNS_DEV` is left set.

**The write queue tracks refused writes separately from transient failures.**
A retryable error clears on the next success; a write the server rejected
outright is a tap that will never be recorded, and the scoring screen says so
rather than letting it vanish.

### Verified

- Go: `go vet`, `golangci-lint`, `gosec`, `govulncheck`, tests under `-race`.
  The scoring engine is at 100% statement coverage.
- Web: 19 unit tests over the queue and cue scheduling, `biome`,
  `html-validate`.
- End to end: 7 Playwright tests on a phone viewport against the real API,
  including going offline mid-round, tapping four further throws, and
  reconciling to an exact total on reconnect.
- Cross-cutting: CodeQL, Semgrep, Trivy, jscpd.

### Outstanding

- **HTTPS enforcement on Pages.** The certificate did not re-provision after
  the repository rename; GitHub's API still reports "the certificate does not
  exist yet" while serving HTTPS perfectly well. Toggle it in Settings → Pages
  once it appears.
- **SonarCloud** needs the project created and `SONAR_TOKEN` added. The job
  skips itself until then rather than failing.
- **The API is not deployed yet.** `deploy/README.md` covers the tunnel, the
  Mailgun DNS records and the first invite.
- **Phase two:** season standings, handicap application, the mulligan drop and
  promotion tracking. The schema and `ScoreWeek`/`ScoreSeason` already handle
  all of it; nothing surfaces it.
- **One rule interpretation still worth confirming with a league official:**
  that a zero-point catch carries no bonus at all.
