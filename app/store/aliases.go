package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrSameIngredient means that the two names already stand for one
// ingredient.
var ErrSameIngredient = errors.New("the names already stand for the same ingredient")

// Alias is another name for an ingredient.
type Alias struct {
	Alias string // such as 西红柿
	Name  string // the ingredient's own name, such as 番茄
}

// AddAlias makes alias another name for the ingredient that name stands
// for, and returns that ingredient's own name. If alias was an ingredient
// of its own, it is merged in: the cooks that had it get the ingredient
// instead, and its aliases follow. moved is how many cooks that was.
func (s *Store) AddAlias(ctx context.Context, alias, name string) (canonical string, moved int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()

	target, err := ingredientID(ctx, tx, name)
	if err != nil {
		return "", 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT name FROM ingredients WHERE id = ?`, target).Scan(&canonical); err != nil {
		return "", 0, err
	}
	var current int64 // what alias stands for now, if anything
	err = tx.QueryRowContext(ctx, `
		SELECT ingredient_id FROM ingredient_aliases WHERE alias = ?1
		UNION ALL
		SELECT id FROM ingredients WHERE name = ?1`, alias).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", 0, err
	case current == target:
		return "", 0, ErrSameIngredient
	}

	var old int64 // alias as an ingredient of its own
	err = tx.QueryRowContext(ctx, `SELECT id FROM ingredients WHERE name = ?`, alias).Scan(&old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", 0, err
	default:
		if moved, err = merge(ctx, tx, old, target); err != nil {
			return "", 0, err
		}
	}
	// An alias that pointed elsewhere is pointed here. Cooks saved under
	// it before stay with the ingredient it stood for then.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ingredient_aliases (alias, ingredient_id) VALUES (?, ?)
		ON CONFLICT (alias) DO UPDATE SET ingredient_id = excluded.ingredient_id`, alias, target); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	return canonical, moved, nil
}

// merge folds ingredient old into ingredient into, and returns how many
// cooks it moved.
func merge(ctx context.Context, tx *sql.Tx, old, into int64) (int, error) {
	var moved int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cook_ingredients WHERE ingredient_id = ?`, old).Scan(&moved); err != nil {
		return 0, err
	}
	steps := []struct {
		query string
		args  []any
	}{
		// A cook that had both names keeps one link.
		{`INSERT INTO cook_ingredients (cook_id, ingredient_id)
		  SELECT cook_id, ? FROM cook_ingredients WHERE ingredient_id = ?
		  ON CONFLICT DO NOTHING`, []any{into, old}},
		{`DELETE FROM cook_ingredients WHERE ingredient_id = ?`, []any{old}},
		{`UPDATE ingredient_aliases SET ingredient_id = ? WHERE ingredient_id = ?`, []any{into, old}},
		{`DELETE FROM ingredients WHERE id = ?`, []any{old}},
	}
	for _, step := range steps {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return 0, err
		}
	}
	return moved, nil
}

// Aliases lists every alias, by ingredient and then by alias.
func (s *Store) Aliases(ctx context.Context) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.alias, i.name
		  FROM ingredient_aliases a JOIN ingredients i ON i.id = a.ingredient_id
		 ORDER BY i.name, a.alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alias
	for rows.Next() {
		var a Alias
		if err := rows.Scan(&a.Alias, &a.Name); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
