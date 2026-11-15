package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// cook is a short way to write a Cook in a test: rating < 0 means none.
func cook(dish, on string, rating float64, note string, ingredients ...string) Cook {
	c := Cook{Dish: dish, Ingredients: ingredients, Note: note, CookedOn: on, AuthorID: "u1", Source: dish}
	if rating >= 0 {
		c.Rating = &rating
	}
	return c
}

func (s *Store) save(t *testing.T, cooks ...Cook) {
	t.Helper()
	for _, c := range cooks {
		if _, err := s.SaveCook(context.Background(), c, at); err != nil {
			t.Fatalf("%s: %v", c.Dish, err)
		}
	}
}

// with lists DishesWith's answer for name, sorted by dish, one line each:
// dish|times|rating|last|note.
func (s *Store) with(t *testing.T, name string) []string {
	t.Helper()
	dishes, err := s.DishesWith(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range dishes {
		r := "-"
		if d.Rating != nil {
			r = fmt.Sprint(*d.Rating)
		}
		out = append(out, fmt.Sprintf("%s|%d|%s|%s|%s", d.Dish, d.Times, r, d.Last, d.Note))
	}
	slices.Sort(out)
	return out
}

func wantLines(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n%q\nwant\n%q", what, got, want)
	}
}

func TestDishesWith(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	s.save(t,
		cook("麻婆豆腐", "2026-11-01", 8, "花椒放少了", "豆腐", "牛肉末"),
		cook("麻婆豆腐", "2026-11-11", 9, "", "豆腐", "猪肉末"),
		// Saved last, cooked first: its note is not the latest.
		cook("麻婆豆腐", "2026-10-20", -1, "太咸", "豆腐"),
		cook("家常豆腐", "2026-11-12", -1, "", "豆腐", "青椒"),
		cook("鱼香茄子", "2026-11-13", 9, "", "茄子", "猪肉末"),
		// Without 猪肉末 this time; it still counts for the dish.
		cook("鱼香茄子", "2026-11-14", 7, "", "茄子"),
	)
	wantLines(t, "豆腐", s.with(t, "豆腐"),
		"家常豆腐|1|-|2026-11-12|",
		"麻婆豆腐|3|8.5|2026-11-11|花椒放少了",
	)
	wantLines(t, "猪肉末", s.with(t, "猪肉末"),
		"鱼香茄子|2|8|2026-11-14|",
		"麻婆豆腐|3|8.5|2026-11-11|花椒放少了",
	)
	wantLines(t, "牛肉末", s.with(t, "牛肉末"), "麻婆豆腐|3|8.5|2026-11-11|花椒放少了")
	wantLines(t, "an ingredient never used", s.with(t, "榴莲"))
	// Whole names only: 豆 is not 豆腐.
	wantLines(t, "part of a name", s.with(t, "豆"))
}

// aliases lists the aliases as "alias=name".
func (s *Store) aliases(t *testing.T) []string {
	t.Helper()
	list, err := s.Aliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Alias+"="+a.Name)
	}
	return out
}

func (s *Store) ingredients(t *testing.T) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT name FROM ingredients ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		out = append(out, name)
	}
	return out
}

func (s *Store) addAlias(t *testing.T, alias, name, wantCanonical string, wantMoved int) {
	t.Helper()
	canonical, moved, err := s.AddAlias(context.Background(), alias, name)
	if err != nil || canonical != wantCanonical || moved != wantMoved {
		t.Fatalf("AddAlias(%s, %s) = %q, %d, %v; want %q, %d", alias, name, canonical, moved, err, wantCanonical, wantMoved)
	}
}

func TestAlias(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	s.save(t,
		cook("番茄炒蛋", "2026-11-10", 8, "", "西红柿", "鸡蛋"),
		cook("番茄蛋汤", "2026-11-11", 7, "", "番茄", "鸡蛋"),
		// Both names in one cook, before anyone said they are the same.
		cook("罗宋汤", "2026-11-12", 9, "", "番茄", "西红柿", "牛肉"),
	)
	// 西红柿 was an ingredient of its own: its two cooks move to 番茄.
	s.addAlias(t, "西红柿", "番茄", "番茄", 2)
	wantLines(t, "ingredients", s.ingredients(t), "牛肉", "番茄", "鸡蛋")
	want := []string{"番茄炒蛋|1|8|2026-11-10|", "番茄蛋汤|1|7|2026-11-11|", "罗宋汤|1|9|2026-11-12|"}
	wantLines(t, "番茄", s.with(t, "番茄"), want...)
	wantLines(t, "西红柿", s.with(t, "西红柿"), want...)

	// From now on 西红柿 is saved as 番茄.
	s.save(t, cook("西红柿炒饭", "2026-11-13", -1, "", "西红柿", "米饭"))
	wantLines(t, "ingredients", s.ingredients(t), "牛肉", "番茄", "米饭", "鸡蛋")
	if got := s.with(t, "番茄"); len(got) != 4 {
		t.Errorf("番茄: %q", got)
	}

	// An alias of an alias is an alias of the ingredient.
	s.addAlias(t, "洋柿子", "西红柿", "番茄", 0)
	wantLines(t, "aliases", s.aliases(t), "洋柿子=番茄", "西红柿=番茄")

	for _, pair := range [][2]string{{"西红柿", "番茄"}, {"番茄", "西红柿"}, {"洋柿子", "西红柿"}, {"番茄", "番茄"}, {"土豆", "土豆"}} {
		if _, _, err := s.AddAlias(context.Background(), pair[0], pair[1]); !errors.Is(err, ErrSameIngredient) {
			t.Errorf("AddAlias(%s, %s): %v", pair[0], pair[1], err)
		}
	}
	// The refused 土豆 = 土豆 left no 土豆 behind, and no transaction open:
	// that would hold the write lock, and this would wait and fail.
	if slices.Contains(s.ingredients(t), "土豆") {
		t.Error("土豆 was added")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.SaveCook(ctx, cook("土豆丝", "2026-11-14", -1, "", "土豆"), at); err != nil {
		t.Errorf("saving after the refusals: %v", err)
	}
}

// When an ingredient that has aliases becomes an alias itself, its
// aliases follow it.
func TestAliasesFollowAMerge(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	s.save(t, cook("番茄炒蛋", "2026-11-10", 8, "", "西红柿", "鸡蛋"))
	s.addAlias(t, "洋柿子", "西红柿", "西红柿", 0)
	s.addAlias(t, "西红柿", "番茄", "番茄", 1)
	wantLines(t, "aliases", s.aliases(t), "洋柿子=番茄", "西红柿=番茄")
	wantLines(t, "洋柿子", s.with(t, "洋柿子"), "番茄炒蛋|1|8|2026-11-10|")
	wantLines(t, "ingredients", s.ingredients(t), "番茄", "鸡蛋")
}

// Pointing an alias somewhere else changes what it means from then on.
func TestAliasRepointed(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	s.addAlias(t, "土豆", "马铃薯", "马铃薯", 0)
	s.addAlias(t, "土豆", "洋芋", "洋芋", 0)
	wantLines(t, "aliases", s.aliases(t), "土豆=洋芋")
}

// A merge that fails halfway leaves everything as it was.
func TestAliasIsAllOrNothing(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "sous.db"))
	s.save(t, cook("番茄炒蛋", "2026-11-10", 8, "", "西红柿", "鸡蛋"))
	if _, err := s.db.Exec(`CREATE TRIGGER keep BEFORE DELETE ON ingredients
		BEGIN SELECT RAISE(ABORT, 'kept'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddAlias(context.Background(), "西红柿", "番茄"); err == nil || !strings.Contains(err.Error(), "kept") {
		t.Fatalf("got %v", err)
	}
	wantLines(t, "ingredients", s.ingredients(t), "西红柿", "鸡蛋")
	wantLines(t, "aliases", s.aliases(t))
	wantLines(t, "西红柿", s.with(t, "西红柿"), "番茄炒蛋|1|8|2026-11-10|")
}
