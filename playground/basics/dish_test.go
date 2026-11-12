package basics

import (
	"maps"
	"testing"
)

func TestSummary(t *testing.T) {
	tests := []struct {
		name string
		dish Dish
		want string
	}{
		{"rated", Dish{Name: "mapo tofu", Ingredients: []string{"tofu", "pork"}, Rating: 9}, "mapo tofu (9/10): tofu, pork"},
		{"unrated", Dish{Name: "omelette", Ingredients: []string{"egg"}}, "omelette (unrated): egg"},
		{"nil ingredients", Dish{Name: "rice", Rating: 5}, "rice (5/10)"},
		{"empty ingredients", Dish{Name: "rice", Ingredients: []string{}, Rating: 5}, "rice (5/10)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.dish.Summary(); got != tt.want {
				t.Errorf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountIngredients(t *testing.T) {
	tests := []struct {
		name   string
		dishes []Dish
		want   map[string]int
	}{
		{"no dishes", nil, map[string]int{}},
		{"dish without ingredients", []Dish{{Name: "rice"}}, map[string]int{}},
		{"shared ingredient", []Dish{
			{Name: "mapo tofu", Ingredients: []string{"tofu", "pork"}},
			{Name: "tofu soup", Ingredients: []string{"tofu", "egg"}},
		}, map[string]int{"tofu": 2, "pork": 1, "egg": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountIngredients(tt.dishes); !maps.Equal(got, tt.want) {
				t.Errorf("CountIngredients() = %v, want %v", got, tt.want)
			}
		})
	}
}
