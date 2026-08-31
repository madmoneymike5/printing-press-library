// Copyright 2026 Jeeves and contributors. Licensed under Apache-2.0.

package cli

import (
	"testing"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/store"
)

func TestDuplicateRecipeGroupsUseTrimmedCaseInsensitiveNames(t *testing.T) {
	groups := duplicateRecipeGroups([]store.RecipeRow{
		{ID: "recipe-2", Name: "  Pancakes"},
		{ID: "recipe-1", Name: "pancakes"},
		{ID: "recipe-3", Name: "Soup"},
		{ID: "recipe-4", Name: "   "},
	})

	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one duplicate group", groups)
	}
	group := groups[0]
	if group.Name != "  Pancakes" || group.Normalized != "pancakes" || group.Count != 2 {
		t.Fatalf("group = %#v, want normalized pancakes with two recipes", group)
	}
	if len(group.Recipes) != 2 || group.Recipes[0].ID != "recipe-2" || group.Recipes[1].ID != "recipe-1" {
		t.Fatalf("recipes = %#v, want both source rows in stable source order", group.Recipes)
	}
}
