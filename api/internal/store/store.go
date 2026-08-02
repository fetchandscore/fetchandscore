// Package store owns the SQLite database: connection setup, schema
// migrations, and the queries the rest of the service runs.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, so the binary needs no cgo
)

// Store is a handle on the database.
type Store struct {
	db *sql.DB
}

// DB exposes the underlying handle. Prefer the typed methods on Store; this
// exists for migrations, tests, and the occasional one-off.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// pragmas are applied to every connection in the pool.
//
// SQLite defaults are tuned for a 2004 desktop, not for a server that must
// keep serving readers while a scorekeeper writes:
//
//   - foreign_keys is off by default, which silently permits dangling rows.
//   - journal_mode=WAL lets readers continue during a write, which is exactly
//     the live-round access pattern.
//   - busy_timeout makes a contended write wait rather than instantly fail.
//   - synchronous=NORMAL is the accepted pairing with WAL: durable across
//     process crashes, trading only a power-loss window for far fewer fsyncs.
var pragmas = []string{
	`PRAGMA foreign_keys = ON`,
	`PRAGMA journal_mode = WAL`,
	`PRAGMA busy_timeout = 5000`,
	`PRAGMA synchronous = NORMAL`,
}

// Open connects to the SQLite database at path, creating it if needed, and
// brings its schema up to date.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// SQLite tolerates exactly one writer. Holding a single connection makes
	// that constraint explicit and keeps write contention inside Go's pool,
	// where busy_timeout can do its job, rather than in the file lock.
	db.SetMaxOpenConns(1)

	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("applying %q: %w", p, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}
