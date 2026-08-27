package cli

import (
	"context"
	"fmt"

	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/config"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/store"
)

func openLocalStore(flags *rootFlags) (*config.Config, *store.Store, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil, nil, configErr(err)
	}
	st, err := store.Open(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("no local data found — run 'anylist-pp-cli sync' first")
	}
	return cfg, st, nil
}

func openAuthedLocalStore(flags *rootFlags) (*config.Config, *store.Store, error) {
	cfg, st, err := openLocalStore(flags)
	if err != nil {
		return nil, nil, err
	}
	if cfg.AccessToken == "" {
		st.Close()
		return nil, nil, authErr(fmt.Errorf("not authenticated — run 'anylist-pp-cli auth login' first"))
	}
	return cfg, st, nil
}

func syncStoreFromLive(ctx context.Context, cfg *config.Config, st *store.Store) error {
	alClient := anylist.New(cfg)
	userData, err := alClient.GetUserData(ctx)
	if err != nil {
		return err
	}
	return st.SyncFromUserData(userData)
}

func itemRowToPB(item *store.ItemRow, userID string) *pb.ListItem {
	if item == nil {
		return nil
	}
	return &pb.ListItem{
		Identifier:          item.ID,
		ListId:              item.ListID,
		Name:                item.Name,
		Quantity:            item.Quantity,
		Details:             item.Details,
		Checked:             item.Checked,
		Category:            item.Category,
		UserId:              userID,
		CategoryMatchId:     item.CategoryMatchID,
		StoreIds:            item.StoreIDs,
		PhotoIds:            item.PhotoIDs,
		Prices:              item.Prices,
		CategoryAssignments: item.CategoryAssignments,
		ManualSortIndex:     int32(item.SortIndex),
	}
}
