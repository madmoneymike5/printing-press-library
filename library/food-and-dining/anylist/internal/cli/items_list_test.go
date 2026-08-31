// Copyright 2026 Jeeves and contributors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/config"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/store"
)

func TestItemsListUnknownListTableIsEmptyResult(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	st, err := store.Open(&config.Config{Path: configPath})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()

	flags := &rootFlags{configPath: configPath}
	cmd := newItemsListCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--list", "example-resource"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if got := out.String(); got != "No items found in \"example-resource\"\n" {
		t.Fatalf("output = %q, want clear empty-list result", got)
	}
}

func TestItemsListUnknownListJSONIsEmptyArray(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	st, err := store.Open(&config.Config{Path: configPath})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()

	flags := &rootFlags{asJSON: true, configPath: configPath}
	cmd := newItemsListCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--list", "example-resource"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var payload struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if len(payload.Data) != 0 {
		t.Fatalf("output = %q, want empty JSON data array", out.String())
	}
}

func TestItemsListJSONIncludesPackageSize(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	st, err := store.Open(&config.Config{Path: configPath})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()
	if err := st.SyncFromUserData(&pb.PBUserDataResponse{
		ShoppingListsResponse: &pb.ShoppingListsResponse{
			NewLists: []*pb.ShoppingList{{
				Identifier: "list-1",
				Name:       "Groceries",
				Items: []*pb.ListItem{{
					Identifier:      "item-1",
					ListId:          "list-1",
					Name:            "Milk",
					CategoryMatchId: "category-match-1",
					PhotoIds:        []string{"photo-1"},
					StoreIds:        []string{"store-1"},
					Prices:          []*pb.PBItemPrice{{Amount: 4.25, Details: "whole", StoreId: "store-1", Date: "2026-08-30"}},
					CategoryAssignments: []*pb.PBListItemCategoryAssignment{{
						Identifier:      "assignment-1",
						CategoryGroupId: "group-1",
						CategoryId:      "category-1",
					}},
					PackageSizePb: &pb.PBItemPackageSize{
						RawPackageSize: "12 oz carton",
					},
				}},
			}},
		},
	}); err != nil {
		t.Fatalf("SyncFromUserData returned error: %v", err)
	}

	flags := &rootFlags{asJSON: true, configPath: configPath}
	cmd := newItemsListCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--list", "Groceries"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	var payload struct {
		Data []struct {
			PackageSize     string   `json:"package_size"`
			CategoryMatchID string   `json:"category_match_id"`
			PhotoIDs        []string `json:"photo_ids"`
			StoreIDs        []string `json:"store_ids"`
			Prices          []struct {
				Amount  float64 `json:"amount"`
				StoreID string  `json:"store_id"`
			} `json:"prices"`
			CategoryAssignments []struct {
				CategoryID string `json:"category_id"`
			} `json:"category_assignments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if len(payload.Data) != 1 || payload.Data[0].PackageSize != "12 oz carton" || payload.Data[0].CategoryMatchID != "category-match-1" {
		t.Fatalf("output = %q, want package size and category match ID", out.String())
	}
	item := payload.Data[0]
	if len(item.PhotoIDs) != 1 || item.PhotoIDs[0] != "photo-1" || len(item.StoreIDs) != 1 || item.StoreIDs[0] != "store-1" {
		t.Fatalf("metadata IDs = %#v, want photo and store IDs", item)
	}
	if len(item.Prices) != 1 || item.Prices[0].Amount != 4.25 || item.Prices[0].StoreID != "store-1" {
		t.Fatalf("prices = %#v, want the cached price", item.Prices)
	}
	if len(item.CategoryAssignments) != 1 || item.CategoryAssignments[0].CategoryID != "category-1" {
		t.Fatalf("category assignments = %#v, want the cached assignment", item.CategoryAssignments)
	}
}
