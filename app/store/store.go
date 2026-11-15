// Package store keeps the cooking log in SQLite.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // the "sqlite" driver, in pure Go: no cgo
)

//go:embed schema_v1.sql
var schemaV1 string

// migrations take the schema from one version to the next, and the
// database's user_version counts how many have run. Only ever append: a
// migration that has run somewhere must not change.
var migrations = []string{schemaV1}

// Store is the app's database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it and its directory if need
// be, and brings its schema up to date.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// The pragmas go in the DSN so that every connection in the pool gets
	// them: foreign keys are off by default, and the setting is per
	// connection. WAL lets reads go on while a write is in progress, and
	// busy_timeout makes a second writer wait instead of failing at once.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate runs the migrations that the database has not had yet, each in a
// transaction of its own together with the new user_version.
func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("the schema is at version %d, newer than this program's %d", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		// A pragma takes no query parameters; v is an int, so this is safe.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
