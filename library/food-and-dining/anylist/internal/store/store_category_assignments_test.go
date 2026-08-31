// Copyright 2026 Jeeves and contributors. Licensed under Apache-2.0.

package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/config"
)

func TestOpenMigratesCategoryAssignmentsColumn(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(tempDir, "anylist.db"))
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE items (
		id TEXT PRIMARY KEY,
		list_id TEXT NOT NULL,
		name TEXT NOT NULL,
		product_upc TEXT DEFAULT '',
		quantity TEXT DEFAULT '',
		details TEXT DEFAULT '',
		category TEXT DEFAULT '',
		category_match_id TEXT DEFAULT '',
		checked INTEGER NOT NULL DEFAULT 0,
		manual_sort_index INTEGER DEFAULT 0,
		store_ids TEXT DEFAULT '[]',
		prices TEXT DEFAULT '[]',
		photo_ids TEXT DEFAULT '[]'
	)`)
	if err != nil {
		db.Close()
		t.Fatalf("create legacy items table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	st, err := Open(&config.Config{Path: filepath.Join(tempDir, "config.toml")})
	if err != nil {
		t.Fatalf("Open returned error for legacy cache: %v", err)
	}
	defer st.Close()

	var found int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('items') WHERE name = 'category_assignments'`).Scan(&found); err != nil {
		t.Fatalf("check category_assignments column: %v", err)
	}
	if found != 1 {
		t.Fatalf("category_assignments column count = %d, want 1", found)
	}
}

func TestSyncPersistsItemCategoryAssignments(t *testing.T) {
	t.Parallel()

	st, err := Open(&config.Config{Path: filepath.Join(t.TempDir(), "config.toml")})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer st.Close()

	err = st.SyncFromUserData(&pb.PBUserDataResponse{
		ShoppingListsResponse: &pb.ShoppingListsResponse{
			NewLists: []*pb.ShoppingList{{
				Identifier: "list-1",
				Name:       "Groceries",
				Items: []*pb.ListItem{{
					Identifier: "item-1",
					ListId:     "list-1",
					Name:       "Milk",
					CategoryAssignments: []*pb.PBListItemCategoryAssignment{{
						Identifier:      "assignment-1",
						CategoryGroupId: "group-1",
						CategoryId:      "category-1",
					}},
				}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("SyncFromUserData returned error: %v", err)
	}

	item, err := st.FindItemByID("list-1", "item-1")
	if err != nil {
		t.Fatalf("FindItemByID returned error: %v", err)
	}
	if len(item.CategoryAssignments) != 1 || item.CategoryAssignments[0].GetCategoryId() != "category-1" {
		t.Fatalf("cached category assignments = %#v, want the synced assignment", item.CategoryAssignments)
	}
}
