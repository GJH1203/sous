package store

import (
	"context"
	"database/sql"
)

// DishStats sums up the times a dish was cooked.
type DishStats struct {
	Dish   string
	Times  int
	Rating *float64 // the average of the rated times; nil when none was
	Last   string   // the day it was last cooked, YYYY-MM-DD
	Note   string   // the latest note that says something; "" when none
}

// DishesWith returns the dishes that were ever cooked with the ingredient
// that name stands for, in no particular order. The stats count every time
// the dish was cooked, with the ingredient or without: they describe the
// dish.
func (s *Store) DishesWith(ctx context.Context, name string) ([]DishStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH wanted AS (
		    SELECT ingredient_id AS id FROM ingredient_aliases WHERE alias = ?1
		    UNION
		    SELECT id FROM ingredients WHERE name = ?1
		), matching AS (
		    SELECT DISTINCT c.dish_id
		      FROM cooks c JOIN cook_ingredients ci ON ci.cook_id = c.id
		     WHERE ci.ingredient_id IN (SELECT id FROM wanted)
		)
		SELECT d.name, count(*), avg(c.rating), max(c.cooked_on),
		       coalesce((SELECT n.note FROM cooks n
		                  WHERE n.dish_id = d.id AND n.note != ''
		                  ORDER BY n.cooked_on DESC, n.id DESC
		                  LIMIT 1), '')
		  FROM dishes d JOIN cooks c ON c.dish_id = d.id
		 WHERE d.id IN (SELECT dish_id FROM matching)
		 GROUP BY d.id`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DishStats
	for rows.Next() {
		var d DishStats
		var rating sql.NullFloat64 // avg() of no ratings is NULL
		if err := rows.Scan(&d.Dish, &d.Times, &rating, &d.Last, &d.Note); err != nil {
			return nil, err
		}
		if rating.Valid {
			d.Rating = &rating.Float64
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
