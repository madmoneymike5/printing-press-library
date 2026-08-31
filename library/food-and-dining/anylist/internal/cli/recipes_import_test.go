// Copyright 2026 Jeeves and contributors. Licensed under Apache-2.0.

package cli

import "testing"

func TestRecipeImportActionHandlesDuplicatePolicies(t *testing.T) {
	for _, test := range []struct {
		policy string
		count  int
		want   string
	}{
		{policy: "", count: 0, want: "import"},
		{policy: "skip", count: 2, want: "skip"},
		{policy: " UPDATE ", count: 1, want: "update"},
		{policy: "allow", count: 2, want: "import"},
	} {
		got, err := recipeImportAction(test.policy, "Pancakes", test.count)
		if err != nil {
			t.Fatalf("recipeImportAction(%q, %d) returned error: %v", test.policy, test.count, err)
		}
		if got != test.want {
			t.Fatalf("recipeImportAction(%q, %d) = %q, want %q", test.policy, test.count, got, test.want)
		}
	}

	if _, err := recipeImportAction("replace", "Pancakes", 1); err == nil {
		t.Fatal("recipeImportAction accepted unknown policy")
	}
	if _, err := recipeImportAction("update", "Pancakes", 2); err == nil {
		t.Fatal("recipeImportAction allowed an ambiguous update")
	}
}
