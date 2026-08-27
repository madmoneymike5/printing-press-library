package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/config"
)

func currentRecipeData(ctx context.Context, cfg *config.Config) (*pb.PBUserDataResponse, string, error) {
	alClient := anylist.New(cfg)
	userData, err := alClient.GetUserData(ctx)
	if err != nil {
		return nil, "", err
	}
	rdr := userData.GetRecipeDataResponse()
	if rdr == nil || rdr.GetRecipeDataId() == "" {
		return nil, "", fmt.Errorf("recipe data id not found in AnyList user data")
	}
	return userData, rdr.GetRecipeDataId(), nil
}

func findLiveRecipeByName(userData *pb.PBUserDataResponse, name string) (*pb.PBRecipe, error) {
	rdr := userData.GetRecipeDataResponse()
	if rdr == nil {
		return nil, fmt.Errorf("recipe %q not found", name)
	}
	lower := strings.ToLower(name)
	for _, recipe := range rdr.GetRecipes() {
		if strings.EqualFold(recipe.GetName(), name) {
			return recipe, nil
		}
	}
	for _, recipe := range rdr.GetRecipes() {
		if strings.Contains(strings.ToLower(recipe.GetName()), lower) {
			return recipe, nil
		}
	}
	return nil, fmt.Errorf("recipe %q not found", name)
}

// findLiveRecipeExactByName deliberately avoids the fuzzy matching used by
// read-oriented commands. Mutating a similarly named recipe is worse than
// returning a clear miss.
func findLiveRecipeExactByName(userData *pb.PBUserDataResponse, name string) (*pb.PBRecipe, error) {
	matches := findLiveRecipesExactByName(userData, name)
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("recipe %q not found", name)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("recipe %q has %d exact-name duplicates; run 'recipes duplicates' and resolve the ambiguity first", name, len(matches))
	}
}

func findLiveRecipesExactByName(userData *pb.PBUserDataResponse, name string) []*pb.PBRecipe {
	rdr := userData.GetRecipeDataResponse()
	if rdr == nil {
		return nil
	}
	var matches []*pb.PBRecipe
	for _, recipe := range rdr.GetRecipes() {
		if normalizeRecipeName(recipe.GetName()) == normalizeRecipeName(name) {
			matches = append(matches, recipe)
		}
	}
	return matches
}

func normalizeRecipeName(name string) string {
	return strings.Join(strings.Fields(strings.ToLower(name)), " ")
}

func currentListFolderData(ctx context.Context, cfg *config.Config) (*pb.PBUserDataResponse, string, string, error) {
	alClient := anylist.New(cfg)
	userData, err := alClient.GetUserData(ctx)
	if err != nil {
		return nil, "", "", err
	}
	lfr := userData.GetListFoldersResponse()
	if lfr == nil || lfr.GetListDataId() == "" {
		return nil, "", "", fmt.Errorf("list folder data id not found in AnyList user data")
	}
	return userData, lfr.GetListDataId(), lfr.GetRootFolderId(), nil
}

func findLiveRecipeCollectionByName(userData *pb.PBUserDataResponse, name string) (*pb.PBRecipeCollection, error) {
	rdr := userData.GetRecipeDataResponse()
	if rdr == nil {
		return nil, fmt.Errorf("recipe collection %q not found", name)
	}
	lower := strings.ToLower(name)
	for _, collection := range rdr.GetRecipeCollections() {
		if strings.EqualFold(collection.GetName(), name) {
			return collection, nil
		}
	}
	for _, collection := range rdr.GetRecipeCollections() {
		if strings.Contains(strings.ToLower(collection.GetName()), lower) {
			return collection, nil
		}
	}
	return nil, fmt.Errorf("recipe collection %q not found", name)
}

func findLiveListFolderByName(userData *pb.PBUserDataResponse, name string) (*pb.PBListFolder, error) {
	lfr := userData.GetListFoldersResponse()
	if lfr == nil {
		return nil, fmt.Errorf("list folder %q not found", name)
	}
	lower := strings.ToLower(name)
	for _, folder := range lfr.GetListFolders() {
		if strings.EqualFold(folder.GetName(), name) {
			return folder, nil
		}
	}
	for _, folder := range lfr.GetListFolders() {
		if strings.Contains(strings.ToLower(folder.GetName()), lower) {
			return folder, nil
		}
	}
	return nil, fmt.Errorf("list folder %q not found", name)
}

func newRecipeID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
