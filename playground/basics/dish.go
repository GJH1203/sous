// Package basics is a warm-up: structs, slices, maps, errors and interfaces.
package basics

import (
	"fmt"
	"strings"
)

// Dish is one thing we cook. A zero Rating means "not rated yet".
type Dish struct {
	Name        string
	Ingredients []string
	Rating      int
}

// Summary only reads the dish, so a value receiver is enough: each call gets
// its own copy of the struct, which is cheap for three fields.
func (d Dish) Summary() string {
	rating := "unrated"
	if d.Rating != 0 {
		rating = fmt.Sprintf("%d/10", d.Rating)
	}
	s := fmt.Sprintf("%s (%s)", d.Name, rating)
	// len is 0 for both a nil and an empty slice, so callers need not care
	// which one they pass.
	if len(d.Ingredients) > 0 {
		s += ": " + strings.Join(d.Ingredients, ", ")
	}
	return s
}

// CountIngredients reports how many times each ingredient appears across dishes.
func CountIngredients(dishes []Dish) map[string]int {
	// Reading a nil map is fine, but writing to one panics, so make it first.
	counts := make(map[string]int)
	for _, d := range dishes {
		for _, ing := range d.Ingredients {
			// A missing key reads as 0, so there is no "if present" branch.
			counts[ing]++
		}
	}
	return counts
}
