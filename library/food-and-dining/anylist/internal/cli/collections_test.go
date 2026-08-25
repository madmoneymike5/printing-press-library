package cli

import (
	"strings"
	"testing"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
)

func TestValidateRecipeCollectionReadBackChecksIdentityMembershipsAndSettings(t *testing.T) {
	t.Parallel()

	expected := &pb.PBRecipeCollection{
		Identifier: "collection-1",
		Name:       "Weeknight dinners",
		RecipeIds:  []string{"recipe-1", "recipe-2"},
		CollectionSettings: &pb.PBRecipeCollectionSettings{
			RecipesSortOrder:                2,
			ShowOnlyRecipesWithNoCollection: true,
		},
	}
	cases := []struct {
		name    string
		mutate  func(*pb.PBRecipeCollection)
		wantErr string
	}{
		{name: "identifier", mutate: func(actual *pb.PBRecipeCollection) { actual.Identifier = "other" }, wantErr: "ID"},
		{name: "name", mutate: func(actual *pb.PBRecipeCollection) { actual.Name = "Other" }, wantErr: "name"},
		{name: "memberships", mutate: func(actual *pb.PBRecipeCollection) { actual.RecipeIds = []string{"recipe-1"} }, wantErr: "memberships"},
		{name: "settings", mutate: func(actual *pb.PBRecipeCollection) { actual.CollectionSettings.RecipesSortOrder = 4 }, wantErr: "settings"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := &pb.PBRecipeCollection{
				Identifier:         expected.Identifier,
				Name:               expected.Name,
				RecipeIds:          append([]string(nil), expected.RecipeIds...),
				CollectionSettings: &pb.PBRecipeCollectionSettings{RecipesSortOrder: 2, ShowOnlyRecipesWithNoCollection: true},
			}
			tc.mutate(actual)
			if err := validateRecipeCollectionReadBack(expected, actual); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateRecipeCollectionReadBack error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}

	actual := &pb.PBRecipeCollection{
		Identifier: expected.Identifier,
		Name:       expected.Name,
		RecipeIds:  append([]string(nil), expected.RecipeIds...),
		CollectionSettings: &pb.PBRecipeCollectionSettings{
			RecipesSortOrder:                2,
			ShowOnlyRecipesWithNoCollection: true,
		},
		Timestamp: expected.Timestamp + 1,
	}
	if err := validateRecipeCollectionReadBack(expected, actual); err != nil {
		t.Fatalf("validateRecipeCollectionReadBack returned error for matching live collection: %v", err)
	}
}

func TestFindLiveRecipeCollectionByID(t *testing.T) {
	t.Parallel()

	data := &pb.PBUserDataResponse{RecipeDataResponse: &pb.PBRecipeDataResponse{
		RecipeCollections: []*pb.PBRecipeCollection{{Identifier: "collection-1", Name: "Dinners"}},
	}}
	collection, found := findLiveRecipeCollectionByID(data, "collection-1")
	if !found || collection.GetName() != "Dinners" {
		t.Fatalf("findLiveRecipeCollectionByID found = %v, collection = %#v", found, collection)
	}
	if _, found := findLiveRecipeCollectionByID(data, "missing"); found {
		t.Fatal("findLiveRecipeCollectionByID found a missing collection")
	}
}
