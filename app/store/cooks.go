package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Cook is one time a dish was cooked.
type Cook struct {
	Dish        string
	Ingredients []string
	Rating      *float64 // nil when not rated
	Note        string
	CookedOn    string // YYYY-MM-DD, in the app's time zone
	AuthorID    string // the Discord user who recorded it
	Source      string // what the user said about it
}

// SaveCook stores c, recorded at the instant at, and says how many times
// its dish has been cooked, this time included. It is one transaction: a
// failure halfway leaves no dish without a cook, and no cook without its
// ingredients.
func (s *Store) SaveCook(ctx context.Context, c Cook, at time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// After a commit, Rollback does nothing; before one, it undoes
	// everything. So this one line covers every return below.
	defer tx.Rollback()

	created := at.UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dishes (name, created_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`,
		c.Dish, created); err != nil {
		return 0, err
	}
	var dishID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM dishes WHERE name = ?`, c.Dish).Scan(&dishID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO cooks (dish_id, cooked_on, rating, note, author_id, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		dishID, c.CookedOn, c.Rating, c.Note, c.AuthorID, c.Source, created)
	if err != nil {
		return 0, err
	}
	cookID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, name := range c.Ingredients {
		id, err := ingredientID(ctx, tx, name)
		if err != nil {
			return 0, err
		}
		// Two names can be one ingredient, so a link may already be there.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cook_ingredients (cook_id, ingredient_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			cookID, id); err != nil {
			return 0, err
		}
	}
	var times int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cooks WHERE dish_id = ?`, dishID).Scan(&times); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return times, nil
}

// ingredientID returns the ingredient that name stands for: the one it is
// an alias of, or else the one called name, which is added if it is new.
func ingredientID(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT ingredient_id FROM ingredient_aliases WHERE alias = ?`, name).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		return id, err // an alias, or a failure
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ingredients (name) VALUES (?) ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return 0, err
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM ingredients WHERE name = ?`, name).Scan(&id)
	return id, err
}
