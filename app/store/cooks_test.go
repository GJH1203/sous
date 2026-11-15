package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// cooks lists every cook, oldest first, as one line each:
// dish|cooked_on|rating|note|author|source|created_at|ingredients.
func (s *Store) cooks(t *testing.T) []string {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT d.name, c.cooked_on, c.rating, c.note, c.author_id, c.source, c.created_at,
		       (SELECT group_concat(i.name, ',' ORDER BY i.name)
		          FROM cook_ingredients ci JOIN ingredients i ON i.id = ci.ingredient_id
		         WHERE ci.cook_id = c.id)
		  FROM cooks c JOIN dishes d ON d.id = c.dish_id
		 ORDER BY c.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dish, on, note, author, source, created string
		var rating sql.NullFloat64
		var ingredients sql.NullString
		if err := rows.Scan(&dish, &on, &rating, &note, &author, &source, &created, &ingredients); err != nil {
			t.Fatal(err)
		}
		r := "-"
		if rating.Valid {
			r = fmt.Sprint(rating.Float64)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s", dish, on, r, note, author, source, created, ingredients.String))
	}
	return out
}

func (s *Store) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// at is a Saturday evening west of UTC, where it is already Sunday.
var at = time.Date(2026, 11, 14, 20, 30, 0, 0, time.FixedZone("UTC-8", -8*3600))

func TestSaveCook(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	ctx := context.Background()
	nine := 9.0
	save := func(c Cook, at time.Time, want int) {
		t.Helper()
		times, err := s.SaveCook(ctx, c, at)
		if err != nil || times != want {
			t.Fatalf("%s: %d times, %v; want %d", c.Dish, times, err, want)
		}
	}
	save(Cook{Dish: "鱼香茄子", Ingredients: []string{"茄子", "猪肉末"}, Rating: &nine, Note: "少放了糖",
		CookedOn: "2026-11-14", AuthorID: "u1", Source: "今天做了鱼香茄子，少放了糖，9 分"}, at, 1)
	// The same dish again, by the other person, with no score.
	save(Cook{Dish: "鱼香茄子", Ingredients: []string{"茄子"}, CookedOn: "2026-11-15", AuthorID: "u2", Source: "鱼香茄子"},
		at.Add(24*time.Hour), 2)
	// A name given twice is one link.
	save(Cook{Dish: "茄子煲", Ingredients: []string{"茄子", "茄子"}, CookedOn: "2026-11-15", AuthorID: "u1", Source: "茄子煲"},
		at.Add(25*time.Hour), 1)

	want := []string{
		"鱼香茄子|2026-11-14|9|少放了糖|u1|今天做了鱼香茄子，少放了糖，9 分|2026-11-15T04:30:00Z|猪肉末,茄子",
		"鱼香茄子|2026-11-15|-||u2|鱼香茄子|2026-11-16T04:30:00Z|茄子",
		"茄子煲|2026-11-15|-||u1|茄子煲|2026-11-16T05:30:00Z|茄子",
	}
	if got := s.cooks(t); !slices.Equal(got, want) {
		t.Errorf("cooks:\n%q\nwant\n%q", got, want)
	}
	// Dishes and ingredients are shared, not repeated.
	if d, i := s.count(t, "dishes"), s.count(t, "ingredients"); d != 2 || i != 2 {
		t.Errorf("%d dishes, %d ingredients; want 2, 2", d, i)
	}
}

// A failure at any step leaves nothing behind: no dish without a cook, no
// ingredient without a link.
func TestSaveCookIsAllOrNothing(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	ctx := context.Background()
	// The last step fails: the second ingredient, after the dish, the cook
	// and the first ingredient are in.
	if _, err := s.db.Exec(`CREATE TRIGGER no_onions BEFORE INSERT ON ingredients
		WHEN NEW.name = '洋葱' BEGIN SELECT RAISE(ABORT, 'no onions'); END`); err != nil {
		t.Fatal(err)
	}
	eleven := 11.0
	for name, c := range map[string]Cook{
		"failing ingredient": {Dish: "洋葱炒蛋", Ingredients: []string{"鸡蛋", "洋葱"}, CookedOn: "2026-11-14", AuthorID: "u1", Source: "洋葱炒蛋"},
		"rating above 10":    {Dish: "番茄炒蛋", Ingredients: []string{"番茄"}, Rating: &eleven, CookedOn: "2026-11-14", AuthorID: "u1", Source: "番茄炒蛋"},
	} {
		if _, err := s.SaveCook(ctx, c, at); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	for _, table := range []string{"dishes", "cooks", "ingredients", "cook_ingredients"} {
		if n := s.count(t, table); n != 0 {
			t.Errorf("%d rows left in %s", n, table)
		}
	}
	// A transaction left open would hold the write lock, and this would
	// wait for busy_timeout and fail.
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := s.SaveCook(ctx, Cook{Dish: "番茄炒蛋", Ingredients: []string{"番茄"}, CookedOn: "2026-11-14", AuthorID: "u1", Source: "番茄炒蛋"}, at); err != nil {
		t.Errorf("saving after the failures: %v", err)
	}
}
