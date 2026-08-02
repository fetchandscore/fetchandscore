package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp returns a store backed by a fresh database file that is removed
// when the test ends.
func openTemp(t *testing.T) *Store {
	t.Helper()

	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_AppliesMigrations(t *testing.T) {
	s := openTemp(t)

	// The users table is created by the first migration; querying it proves
	// the migrations ran rather than merely that the file opened.
	var n int
	if err := s.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("querying users: %v", err)
	}
	if n != 0 {
		t.Errorf("new database has %d users, want 0", n)
	}
}

func TestOpen_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first: %v", err)
	}

	// Re-opening an already-migrated database must not try to re-apply.
	second, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = second.Close() }()

	var applied int
	err = second.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM schema_migrations`).Scan(&applied)
	if err != nil {
		t.Fatalf("counting migrations: %v", err)
	}
	if applied == 0 {
		t.Fatal("no migrations recorded as applied")
	}
}

// Foreign keys are off by default in SQLite and must be enabled per
// connection. A dangling reference should be rejected, not silently stored.
func TestOpen_EnforcesForeignKeys(t *testing.T) {
	s := openTemp(t)

	// Every column is supplied, so the only thing left to reject this row is
	// the dangling club_id and user_id.
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO club_members (club_id, user_id, role, joined_at) VALUES (999, 999, 'member', 0)`)
	if err == nil {
		t.Fatal("inserting a member of a nonexistent club succeeded, want a foreign key error")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("insert failed with %v, want a foreign key violation", err)
	}
}

// Write-ahead logging lets the SSE readers keep reading while a scorekeeper
// writes, which is the whole access pattern of a live round.
func TestOpen_UsesWriteAheadLogging(t *testing.T) {
	s := openTemp(t)

	var mode string
	if err := s.DB().QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("reading journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}
