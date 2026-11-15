package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (s *Store) userVersion(t *testing.T) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOpenCreatesTheSchema(t *testing.T) {
	// A directory that does not exist yet.
	s := open(t, filepath.Join(t.TempDir(), "a", "b", "sous.db"))
	rows, err := s.db.Query("SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	want := []string{"cook_ingredients", "cooks", "dishes", "ingredient_aliases", "ingredients"}
	if !slices.Equal(tables, want) {
		t.Errorf("tables %v, want %v", tables, want)
	}
	if v := s.userVersion(t); v != len(migrations) {
		t.Errorf("user_version %d, want %d", v, len(migrations))
	}
	var mode string
	s.db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode %q", mode)
	}
}

// Foreign keys are a per-connection setting, so every connection in the
// pool must have it.
func TestEveryConnectionChecksForeignKeys(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	ctx := context.Background()
	var conns []interface{ Close() error }
	for range 3 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		var on int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil || on != 1 {
			t.Errorf("foreign_keys = %d, %v", on, err)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sous.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO dishes (name, created_at) VALUES ('番茄炒蛋', '2026-11-14T03:04:05Z')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open(t, path)
	var n int
	s.db.QueryRow("SELECT count(*) FROM dishes").Scan(&n)
	if n != 1 || s.userVersion(t) != len(migrations) {
		t.Errorf("after reopening: %d dishes, version %d", n, s.userVersion(t))
	}
}

func TestNewerSchemaIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sous.db")
	s := open(t, path)
	s.db.Exec("PRAGMA user_version = 99")
	s.Close()
	_, err := Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "version 99, newer than this program's 1") {
		t.Errorf("got %v", err)
	}
}

// A migration that fails halfway leaves nothing behind, and the version
// where it was.
func TestFailedMigrationRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sous.db")
	open(t, path).Close()
	saved := migrations
	t.Cleanup(func() { migrations = saved })
	// Two statements: the first works, the second fails.
	migrations = append(slices.Clone(saved), "CREATE TABLE half (id INTEGER); SELECT * FROM no_such_table;")
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("got %v", err)
	}
	migrations = saved
	s := open(t, path)
	var n int
	s.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'half'").Scan(&n)
	if n != 0 || s.userVersion(t) != 1 {
		t.Errorf("after the failed migration: table half %d, version %d", n, s.userVersion(t))
	}
}

func TestConstraints(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	mustExec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO dishes (id, name, created_at) VALUES (1, '鱼香茄子', 'x')`)
	mustExec(`INSERT INTO ingredients (id, name) VALUES (1, '茄子')`)
	mustExec(`INSERT INTO cooks (id, dish_id, cooked_on, rating, author_id, source, created_at)
		VALUES (1, 1, '2026-11-13', 9, 'u', '今天做了鱼香茄子', 'x')`)
	mustExec(`INSERT INTO cook_ingredients VALUES (1, 1)`)

	for name, q := range map[string]string{
		"unknown dish":      `INSERT INTO cooks (dish_id, cooked_on, author_id, source, created_at) VALUES (2, '2026-11-13', 'u', '', 'x')`,
		"rating above 10":   `INSERT INTO cooks (dish_id, cooked_on, rating, author_id, source, created_at) VALUES (1, '2026-11-13', 11, 'u', '', 'x')`,
		"time, not a date":  `INSERT INTO cooks (dish_id, cooked_on, author_id, source, created_at) VALUES (1, '2026-11-13T20:00:00Z', 'u', '', 'x')`,
		"dish named twice":  `INSERT INTO dishes (name, created_at) VALUES ('鱼香茄子', 'x')`,
		"unknown alias to":  `INSERT INTO ingredient_aliases VALUES ('西红柿', 2)`,
		"ingredient twice":  `INSERT INTO cook_ingredients VALUES (1, 1)`,
		"dish with cooks":   `DELETE FROM dishes WHERE id = 1`,
		"no source message": `INSERT INTO cooks (dish_id, cooked_on, author_id, created_at) VALUES (1, '2026-11-13', 'u', 'x')`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// Deleting a cook takes its ingredient links with it.
	mustExec(`DELETE FROM cooks WHERE id = 1`)
	var n int
	s.db.QueryRow("SELECT count(*) FROM cook_ingredients").Scan(&n)
	if n != 0 {
		t.Errorf("%d links left after deleting the cook", n)
	}
}
