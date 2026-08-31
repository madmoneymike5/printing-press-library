// Copyright 2026 Jeeves and contributors. Licensed under Apache-2.0.
// Hand-patched after generation: duplicate reporting supports safe recipe cleanup.

package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/store"
	"github.com/spf13/cobra"
)

type recipeDuplicateGroup struct {
	Name       string                 `json:"name"`
	Normalized string                 `json:"normalized_name"`
	Count      int                    `json:"count"`
	Recipes    []recipeDuplicateEntry `json:"recipes"`
}

type recipeDuplicateEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SourceName string `json:"source_name,omitempty"`
	SourceURL  string `json:"source_url,omitempty"`
}

func newRecipesDuplicatesCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "duplicates",
		Short:       "Report recipes with duplicate names without changing anything",
		Example:     "  anylist-pp-cli recipes duplicates --json",
		Annotations: map[string]string{"pp:endpoint": "recipes.duplicates", "pp:method": "GET", "pp:path": "local-cache", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return nil
			}
			_, st, err := openLocalStore(flags)
			if err != nil {
				return err
			}
			defer st.Close()
			recipes, err := st.GetRecipes()
			if err != nil {
				return fmt.Errorf("reading recipes: %w", err)
			}
			groups := duplicateRecipeGroups(recipes)
			if flags.asJSON {
				return printJSONWithFreshness(cmd.OutOrStdout(), groups, flags)
			}
			if len(groups) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No duplicate recipe names found")
				return nil
			}
			tw := newTabWriter(cmd.OutOrStdout())
			fmt.Fprintln(tw, "NAME\tCOUNT\tRECIPE IDS")
			for _, group := range groups {
				ids := make([]string, 0, len(group.Recipes))
				for _, recipe := range group.Recipes {
					ids = append(ids, recipe.ID)
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\n", group.Name, group.Count, strings.Join(ids, ","))
			}
			return tw.Flush()
		},
	}
	return cmd
}

func duplicateRecipeGroups(recipes []store.RecipeRow) []recipeDuplicateGroup {
	byName := make(map[string][]store.RecipeRow)
	for _, recipe := range recipes {
		key := strings.ToLower(strings.TrimSpace(recipe.Name))
		if key != "" {
			byName[key] = append(byName[key], recipe)
		}
	}
	keys := make([]string, 0, len(byName))
	for key, group := range byName {
		if len(group) > 1 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	groups := make([]recipeDuplicateGroup, 0, len(keys))
	for _, key := range keys {
		group := byName[key]
		entries := make([]recipeDuplicateEntry, 0, len(group))
		for _, recipe := range group {
			entries = append(entries, recipeDuplicateEntry{ID: recipe.ID, Name: recipe.Name, SourceName: recipe.SourceName, SourceURL: recipe.SourceURL})
		}
		groups = append(groups, recipeDuplicateGroup{Name: group[0].Name, Normalized: key, Count: len(group), Recipes: entries})
	}
	return groups
}
