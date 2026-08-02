-- Initial schema for slice 1: score a round end to end.
--
-- Conventions:
--   * Timestamps are INTEGER Unix milliseconds, UTC.
--   * Scores are INTEGER half-points (see internal/scoring). Columns carrying
--     a score are suffixed _x2 so a bare integer is never mistaken for points.
--   * Enumerations are TEXT with a CHECK, so the database is readable by hand
--     and rejects a typo rather than storing it.

CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    email      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    name       TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE clubs (
    id         INTEGER PRIMARY KEY,
    slug       TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE club_members (
    club_id   INTEGER NOT NULL REFERENCES clubs(id) ON DELETE CASCADE,
    user_id   INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role      TEXT    NOT NULL CHECK (role IN ('member', 'scorekeeper', 'captain', 'admin')),
    joined_at INTEGER NOT NULL,
    PRIMARY KEY (club_id, user_id)
) STRICT;

-- Membership is by invitation: only an invited address may request a sign-in
-- link, which keeps the mail endpoint from becoming an open relay.
CREATE TABLE invites (
    id          INTEGER PRIMARY KEY,
    club_id     INTEGER NOT NULL REFERENCES clubs(id) ON DELETE CASCADE,
    email       TEXT    NOT NULL COLLATE NOCASE,
    role        TEXT    NOT NULL CHECK (role IN ('member', 'scorekeeper', 'captain', 'admin')),
    token_hash  BLOB    NOT NULL UNIQUE,
    invited_by  INTEGER          REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    accepted_at INTEGER
) STRICT;

CREATE INDEX idx_invites_email ON invites (email) WHERE accepted_at IS NULL;

-- Sign-in links. The token itself is never stored, only its hash, so a
-- database leak cannot be replayed into a session.
CREATE TABLE login_tokens (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BLOB    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
) STRICT;

CREATE TABLE auth_sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   BLOB    NOT NULL UNIQUE,
    user_agent   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_auth_sessions_user ON auth_sessions (user_id);

CREATE TABLE dogs (
    id            INTEGER PRIMARY KEY,
    owner_user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT    NOT NULL,
    -- Height at the withers. 40cm or less qualifies as a Tiny Dog; stored
    -- alongside the flag so a re-measure can be justified.
    height_cm     REAL,
    tiny          INTEGER NOT NULL DEFAULT 0 CHECK (tiny IN (0, 1)),
    created_at    INTEGER NOT NULL
) STRICT;

-- A Team is one handler paired with one dog.
CREATE TABLE teams (
    id               INTEGER PRIMARY KEY,
    club_id          INTEGER NOT NULL REFERENCES clubs(id) ON DELETE CASCADE,
    handler_user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    dog_id           INTEGER NOT NULL REFERENCES dogs(id) ON DELETE CASCADE,
    display_name     TEXT    NOT NULL,
    created_at       INTEGER NOT NULL,
    UNIQUE (club_id, handler_user_id, dog_id)
) STRICT;

-- Everything that varies between play formats lives here as data, so the WWC
-- variant or a club's two-minute round is a row rather than a release.
CREATE TABLE seasons (
    id                  INTEGER PRIMARY KEY,
    club_id             INTEGER NOT NULL REFERENCES clubs(id) ON DELETE CASCADE,
    name                TEXT    NOT NULL,
    year                INTEGER NOT NULL,
    format              TEXT    NOT NULL CHECK (format IN ('60_all', '90_5', 'wwc', 'custom')),
    round_seconds       INTEGER NOT NULL,
    rounds_per_week     INTEGER NOT NULL,
    -- How many of a round's highest catches count. 0 means all of them.
    scored_throw_cap    INTEGER NOT NULL DEFAULT 0,
    -- Ceiling on a tiny dog's combined weekly score. 0 means uncapped.
    tiny_weekly_cap_x2  INTEGER NOT NULL DEFAULT 0,
    handicap_junior_x2  INTEGER NOT NULL DEFAULT 0,
    handicap_handler_x2 INTEGER NOT NULL DEFAULT 0,
    handicap_master_x2  INTEGER NOT NULL DEFAULT 0,
    handicap_expert_x2  INTEGER NOT NULL DEFAULT 0,
    -- Remaining-time marks at which the timer speaks, as a JSON array of
    -- seconds in descending order.
    cue_seconds         TEXT    NOT NULL DEFAULT '[30,10,5,4,3,2,1]',
    week_count          INTEGER NOT NULL DEFAULT 5,
    starts_on           TEXT    NOT NULL,
    created_at          INTEGER NOT NULL
) STRICT;

-- A team's registration into one season, carrying the designations that are
-- locked for that season's duration.
CREATE TABLE season_entries (
    id         INTEGER PRIMARY KEY,
    season_id  INTEGER NOT NULL REFERENCES seasons(id) ON DELETE CASCADE,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    division   TEXT    NOT NULL CHECK (division IN ('junior', 'handler', 'master', 'expert')),
    -- The roller designation must be committed for the whole season.
    roller     INTEGER NOT NULL DEFAULT 0 CHECK (roller IN (0, 1)),
    -- Snapshot of the dog's tiny status at registration, so a re-measure
    -- mid-season cannot silently rewrite past scores.
    tiny       INTEGER NOT NULL DEFAULT 0 CHECK (tiny IN (0, 1)),
    created_at INTEGER NOT NULL,
    UNIQUE (season_id, team_id)
) STRICT;

CREATE TABLE weeks (
    id            INTEGER PRIMARY KEY,
    season_id     INTEGER NOT NULL REFERENCES seasons(id) ON DELETE CASCADE,
    idx           INTEGER NOT NULL,
    scheduled_for TEXT    NOT NULL,
    UNIQUE (season_id, idx)
) STRICT;

-- The actual gathering at which rounds are played.
CREATE TABLE play_sessions (
    id         INTEGER PRIMARY KEY,
    club_id    INTEGER NOT NULL REFERENCES clubs(id) ON DELETE CASCADE,
    week_id    INTEGER          REFERENCES weeks(id) ON DELETE SET NULL,
    name       TEXT    NOT NULL DEFAULT '',
    starts_at  INTEGER NOT NULL,
    status     TEXT    NOT NULL DEFAULT 'scheduled'
                       CHECK (status IN ('scheduled', 'active', 'complete', 'cancelled')),
    created_at INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_play_sessions_club_start ON play_sessions (club_id, starts_at);

CREATE TABLE play_session_teams (
    play_session_id  INTEGER NOT NULL REFERENCES play_sessions(id) ON DELETE CASCADE,
    season_entry_id  INTEGER NOT NULL REFERENCES season_entries(id) ON DELETE CASCADE,
    running_order    INTEGER NOT NULL,
    PRIMARY KEY (play_session_id, season_entry_id)
) STRICT;

CREATE TABLE rounds (
    id                  INTEGER PRIMARY KEY,
    play_session_id     INTEGER NOT NULL REFERENCES play_sessions(id) ON DELETE CASCADE,
    season_entry_id     INTEGER NOT NULL REFERENCES season_entries(id) ON DELETE CASCADE,
    round_number        INTEGER NOT NULL,
    -- ready -> running -> grace -> confirmed. The grace state exists because a
    -- throw released before the "T" in TIME is still in play.
    status              TEXT    NOT NULL DEFAULT 'ready'
                                CHECK (status IN ('ready', 'running', 'grace', 'confirmed')),
    started_at          INTEGER,
    ended_at            INTEGER,
    confirmed_at        INTEGER,
    scorekeeper_user_id INTEGER          REFERENCES users(id) ON DELETE SET NULL,
    -- Cached round score, recomputed from throws on every write. The throws
    -- remain the source of truth.
    total_x2            INTEGER NOT NULL DEFAULT 0,
    UNIQUE (play_session_id, season_entry_id, round_number)
) STRICT;

CREATE TABLE throws (
    id          INTEGER PRIMARY KEY,
    round_id    INTEGER NOT NULL REFERENCES rounds(id) ON DELETE CASCADE,
    zone        TEXT    NOT NULL CHECK (zone IN ('miss', '0-10', '10-20', '20-30', '30-40', '40-50', 'out')),
    air         INTEGER NOT NULL DEFAULT 0 CHECK (air IN (0, 1)),
    -- Client-generated identifier making the write idempotent: a retry after
    -- an ambiguous timeout cannot double-score.
    client_id   TEXT    NOT NULL,
    -- Undo is a soft void, so a mis-tap leaves an audit trail.
    void        INTEGER NOT NULL DEFAULT 0 CHECK (void IN (0, 1)),
    -- The client's clock at the moment of the tap, which is the truthful
    -- ordering when a queued batch drains after a dropout.
    recorded_at INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    UNIQUE (round_id, client_id)
) STRICT;

CREATE INDEX idx_throws_round ON throws (round_id, recorded_at, id);

CREATE TABLE audit_log (
    id            INTEGER PRIMARY KEY,
    actor_user_id INTEGER          REFERENCES users(id) ON DELETE SET NULL,
    entity        TEXT    NOT NULL,
    entity_id     INTEGER NOT NULL,
    action        TEXT    NOT NULL,
    detail        TEXT    NOT NULL DEFAULT '',
    at            INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_audit_entity ON audit_log (entity, entity_id, at);
