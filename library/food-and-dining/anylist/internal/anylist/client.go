// Package anylist provides a client for AnyList's unofficial protobuf API.
// Most data endpoints use application/x-www-form-urlencoded with a binary
// protobuf payload in the "operations" field; calendar, folder, starter-list,
// and photo operations use multipart form data. Auth endpoints return JSON.
package anylist

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/anylist/pb"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/food-and-dining/anylist/internal/config"
	"google.golang.org/protobuf/proto"
)

const (
	apiVersion          = "3"
	baseURL             = "https://www.anylist.com"
	maxPhotoUploadBytes = 10 << 20
)

var allowedPhotoContentTypes = map[string]bool{
	"image/avif": true,
	"image/bmp":  true,
	"image/gif":  true,
	"image/jpeg": true,
	"image/png":  true,
	"image/tiff": true,
	"image/webp": true,
}

// PhotoInfo describes a photo after local validation. The upload endpoint
// accepts a multipart part named "photo"; the client always gives that part
// a generated UUID.jpg filename, matching the public web client contract.
type PhotoInfo struct {
	Size        int64
	ContentType string
}

type Client struct {
	cfg        *config.Config
	httpClient *http.Client
	limiter    *cliutil.AdaptiveLimiter
}

// RateLimitError reports an AnyList HTTP 429 without hiding the server's
// retry guidance from callers. The CLI can classify this as a rate-limit
// failure while agents may use RetryAfter to decide when to retry.
type RateLimitError struct {
	RetryAfter string
}

func (e *RateLimitError) Error() string {
	if e == nil || strings.TrimSpace(e.RetryAfter) == "" {
		return "AnyList rate limit exceeded (HTTP 429)"
	}
	return fmt.Sprintf("AnyList rate limit exceeded (HTTP 429); retry after %s", e.RetryAfter)
}

type loginResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func New(cfg *config.Config) *Client {
	rate := cfg.RateLimit
	if rate <= 0 {
		rate = 10
	}
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		limiter:    cliutil.NewAdaptiveLimiter(rate),
	}
}

func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	c.limiter.Wait()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		c.limiter.OnRateLimit()
	} else if resp.StatusCode < http.StatusInternalServerError {
		c.limiter.OnSuccess()
	}
	return resp, nil
}

// EnsureClientIdentifier generates and saves a client identifier if one doesn't
// exist. The identifier is a 32-char hex string sent on every request as
// X-AnyLeaf-Client-Identifier.
func EnsureClientIdentifier(cfg *config.Config) error {
	if cfg.ClientIdentifier != "" {
		return nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating client identifier: %w", err)
	}
	return cfg.SaveClientIdentifier(hex.EncodeToString(b))
}

// Login authenticates with email and password, saving tokens to config.
func (c *Client) Login(ctx context.Context, email, password string) error {
	if err := EnsureClientIdentifier(c.cfg); err != nil {
		return err
	}
	data := url.Values{}
	data.Set("email", email)
	data.Set("password", password)

	// Try the newer /auth/token endpoint first
	resp, err := c.postForm(ctx, "/auth/token", data)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		// Fall back to older /data/validate-login endpoint
		resp.Body.Close()
		resp, err = c.postForm(ctx, "/data/validate-login", data)
		if err != nil {
			return fmt.Errorf("login (fallback): %w", err)
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return fmt.Errorf("decoding login response: %w", err)
	}
	if lr.AccessToken == "" {
		return fmt.Errorf("login response missing access_token; check email and password")
	}
	return c.cfg.SaveAnyListCredentials(lr.AccessToken, lr.RefreshToken, lr.UserID)
}

// RefreshTokens exchanges the stored refresh token for a new access token.
func (c *Client) RefreshTokens(ctx context.Context) error {
	if c.cfg.RefreshToken == "" {
		return fmt.Errorf("no refresh token stored; run 'auth login' first")
	}
	data := url.Values{}
	data.Set("refresh_token", c.cfg.RefreshToken)

	resp, err := c.postForm(ctx, "/auth/token/refresh", data)
	if err != nil {
		return fmt.Errorf("refreshing tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("token refresh failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rr refreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return fmt.Errorf("decoding refresh response: %w", err)
	}
	return c.cfg.SaveAnyListCredentials(rr.AccessToken, rr.RefreshToken, c.cfg.UserID)
}

// doWithRetry sends an authenticated request and retries once on HTTP 401
// by refreshing the token. The builder callback receives the buffered request
// body and must return a fresh request. For requests with no body (GET,
// postRaw), the body slice is nil but the callback must still return a valid
// request.
//
// doWithRetry returns the response for successful (200) and all non-401
// responses; the caller must close resp.Body. It returns an error only when
// the token refresh fails or the retry also produces a 401.
func (c *Client) doWithRetry(ctx context.Context, build func(b []byte) (*http.Request, error), body []byte) (*http.Response, error) {
	req, err := build(body)
	if err != nil {
		return nil, err
	}
	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rateErr := &RateLimitError{RetryAfter: resp.Header.Get("Retry-After")}
		resp.Body.Close()
		return nil, rateErr
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()
	if err := c.RefreshTokens(ctx); err != nil {
		return nil, fmt.Errorf("unauthorized; token refresh failed: %w", err)
	}
	req, err = build(body)
	if err != nil {
		return nil, err
	}
	resp, err = c.doHTTP(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rateErr := &RateLimitError{RetryAfter: resp.Header.Get("Retry-After")}
		resp.Body.Close()
		return nil, rateErr
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		return nil, fmt.Errorf("unauthorized after token refresh")
	}
	return resp, nil
}

// GetUserData fetches all user data (lists, recipes, meal plan, etc.) via a
// single protobuf response from /data/user-data/get.
func (c *Client) GetUserData(ctx context.Context) (*pb.PBUserDataResponse, error) {
	resp, err := c.postRaw(ctx, "/data/user-data/get")
	if err != nil {
		return nil, fmt.Errorf("fetching user data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("user data fetch failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading user data response: %w", err)
	}

	m := &pb.PBUserDataResponse{}
	if err := proto.Unmarshal(body, m); err != nil {
		return nil, fmt.Errorf("decoding user data protobuf: %w", err)
	}
	return m, nil
}

// AddItem adds an item to a shopping list.
func (c *Client) AddItem(ctx context.Context, listID, itemName, quantity, details, category string) error {
	return c.AddItemWithOptions(ctx, listID, itemName, ItemAddOptions{
		Quantity: quantity,
		Details:  details,
		Category: category,
	})
}

// ItemAddOptions contains the structured product metadata AnyList accepts on
// a shopping-list item. ProductUpc is especially useful: AnyList can use it
// to resolve the product name, package size, and thumbnail in its own catalog.
type ItemAddOptions struct {
	Quantity    string
	Details     string
	Category    string
	ProductUpc  string
	PackageSize string
	Price       *pb.PBItemPrice
}

// AddItemWithOptions adds an item and any supplied product metadata to a
// shopping list.
func (c *Client) AddItemWithOptions(ctx context.Context, listID, itemName string, opts ItemAddOptions) error {
	_, err := c.AddItemWithOptionsAndID(ctx, listID, itemName, opts)
	return err
}

// AddItemWithOptionsAndID adds an item and returns the generated item ID so a
// caller can perform an exact fresh read-back or follow-up typed metadata
// operation without guessing which same-named item was created.
func (c *Client) AddItemWithOptionsAndID(ctx context.Context, listID, itemName string, opts ItemAddOptions) (string, error) {
	itemID := strings.ReplaceAll(uuid.NewString(), "-", "")
	op := &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "add-shopping-list-item",
			UserId:      c.cfg.UserID,
		},
		ListId:     listID,
		ListItemId: itemID,
		ListItem: &pb.ListItem{
			Identifier:      itemID,
			ListId:          listID,
			Name:            itemName,
			Checked:         false,
			CategoryMatchId: "other",
			UserId:          c.cfg.UserID,
		},
	}
	if opts.Quantity != "" {
		op.ListItem.Quantity = opts.Quantity
	}
	if opts.Details != "" {
		op.ListItem.Details = opts.Details
	}
	if opts.Category != "" {
		op.ListItem.Category = opts.Category
	}
	if opts.ProductUpc != "" {
		op.ListItem.ProductUpc = opts.ProductUpc
	}
	if opts.PackageSize != "" {
		op.ListItem.PackageSizePb = &pb.PBItemPackageSize{RawPackageSize: opts.PackageSize}
	}
	if opts.Price != nil {
		op.ListItem.Prices = []*pb.PBItemPrice{opts.Price}
	}
	if err := c.sendListOperation(ctx, op); err != nil {
		return "", err
	}
	return itemID, nil
}

// SetItemChecked marks an item as checked or unchecked.
func (c *Client) SetItemChecked(ctx context.Context, listID, itemID string, checked bool) error {
	op := &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "set-list-item-checked",
			UserId:      c.cfg.UserID,
		},
		ListId:     listID,
		ListItemId: itemID,
	}
	if checked {
		op.UpdatedValue = "y"
	} else {
		op.UpdatedValue = "n"
	}
	return c.sendListOperation(ctx, op)
}

// RemoveItem removes an item from a shopping list. AnyList expects the full
// encoded list item on this operation; sending only IDs returns success but is
// ignored by the server.
func (c *Client) RemoveItem(ctx context.Context, listID string, item *pb.ListItem) error {
	op := &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "remove-shopping-list-item",
			UserId:      c.cfg.UserID,
		},
		ListId:     listID,
		ListItemId: item.GetIdentifier(),
		ListItem:   item,
	}
	return c.sendListOperation(ctx, op)
}

// RemoveList deletes a shopping list. AnyList expects this as a protobuf list
// operation; a JSON POST with the same endpoint can return HTTP 200 without
// changing the list.
func (c *Client) RemoveList(ctx context.Context, listID string) error {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	op := &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "remove-shopping-list",
			UserId:      c.cfg.UserID,
		},
		ListId: listID,
	}
	return c.sendListOperation(ctx, op)
}

// CreateList creates a shopping list using AnyList's protobuf operation. The
// returned list is the requested operation payload; callers must read it back
// before reporting success because the endpoint can acknowledge an operation
// without making the list visible.
func (c *Client) CreateList(ctx context.Context, name string) (*pb.ShoppingList, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("list name must not be empty")
	}
	list := &pb.ShoppingList{
		Identifier:                       strings.ReplaceAll(uuid.NewString(), "-", ""),
		Timestamp:                        float64(time.Now().Unix()),
		Name:                             name,
		Creator:                          c.cfg.UserID,
		LogicalClockTime:                 1,
		AllowsMultipleListCategoryGroups: true,
		ListItemSortOrder:                int32(pb.ShoppingList_Manual),
		NewListItemPosition:              int32(pb.ShoppingList_Bottom),
	}
	op := &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "new-shopping-list",
			UserId:      c.cfg.UserID,
		},
		ListId: list.GetIdentifier(),
		List:   list,
	}
	if err := c.sendListOperation(ctx, op); err != nil {
		return nil, err
	}
	return list, nil
}

// RenameList changes only a shopping list's name. Callers provide the full
// live list payload (with its name already changed) so all other fields are
// preserved exactly; the server uses originalValue/updatedValue as the
// mutation envelope.
func (c *Client) RenameList(ctx context.Context, listID, originalName, newName string, list *pb.ShoppingList) error {
	listID = strings.TrimSpace(listID)
	originalName = strings.TrimSpace(originalName)
	newName = strings.TrimSpace(newName)
	if listID == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if originalName == "" {
		return fmt.Errorf("original list name must not be empty")
	}
	if newName == "" {
		return fmt.Errorf("new list name must not be empty")
	}
	if list == nil {
		return fmt.Errorf("list payload must not be nil")
	}
	if list.GetIdentifier() != "" && list.GetIdentifier() != listID {
		return fmt.Errorf("list payload ID %q does not match list ID %q", list.GetIdentifier(), listID)
	}
	return c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "rename-list",
			UserId:      c.cfg.UserID,
		},
		ListId:        listID,
		OriginalValue: originalName,
		UpdatedValue:  newName,
		List:          list,
	})
}

// UpdateItemFields updates one or more scalar fields of an existing list item.
func (c *Client) UpdateItemFields(ctx context.Context, listID, itemID string, fields map[string]string) error {
	handlerIDs := map[string]string{
		"name":              "set-list-item-name",
		"quantity":          "set-list-item-quantity",
		"details":           "set-list-item-details",
		"category_match_id": "set-list-item-category-match-id",
		"product_upc":       "set-list-item-product-upc",
	}
	ops := &pb.PBListOperationList{}
	for field, value := range fields {
		handlerID, ok := handlerIDs[field]
		if !ok {
			return fmt.Errorf("unsupported item update field %q", field)
		}
		ops.Operations = append(ops.Operations, &pb.PBListOperation{
			Metadata: &pb.PBOperationMetadata{
				OperationId: uuid.NewString(),
				HandlerId:   handlerID,
				UserId:      c.cfg.UserID,
			},
			ListId:       listID,
			ListItemId:   itemID,
			UpdatedValue: value,
		})
	}
	if len(ops.Operations) == 0 {
		return nil
	}
	return c.sendListOperations(ctx, ops)
}

// AddListNotificationLocation adds a location to a shopping list and returns
// the stable location ID used in the operation. The caller must verify the
// location through a fresh user-data read before reporting success.
func (c *Client) AddListNotificationLocation(ctx context.Context, listID string, location *pb.PBNotificationLocation) (string, error) {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return "", fmt.Errorf("list ID must not be empty")
	}
	if location == nil {
		return "", fmt.Errorf("notification location must not be nil")
	}
	updated := proto.Clone(location).(*pb.PBNotificationLocation)
	if strings.TrimSpace(updated.GetIdentifier()) == "" {
		updated.Identifier = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if strings.TrimSpace(updated.GetName()) == "" || strings.TrimSpace(updated.GetAddress()) == "" {
		return "", fmt.Errorf("notification location requires a name and address")
	}
	if updated.GetLatitude() < -90 || updated.GetLatitude() > 90 || updated.GetLongitude() < -180 || updated.GetLongitude() > 180 {
		return "", fmt.Errorf("notification location coordinates are out of range")
	}
	if err := c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{OperationId: uuid.NewString(), HandlerId: "add-list-notification-location", UserId: c.cfg.UserID},
		ListId:   listID, NotificationLocation: updated,
	}); err != nil {
		return "", err
	}
	return updated.GetIdentifier(), nil
}

// RemoveListNotificationLocation removes a location by stable ID. The caller
// must verify its absence through a fresh user-data read before success.
func (c *Client) RemoveListNotificationLocation(ctx context.Context, listID string, location *pb.PBNotificationLocation) error {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if location == nil || strings.TrimSpace(location.GetIdentifier()) == "" {
		return fmt.Errorf("notification location must include an identifier")
	}
	return c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{OperationId: uuid.NewString(), HandlerId: "remove-list-notification-location", UserId: c.cfg.UserID},
		ListId:   listID, NotificationLocation: proto.Clone(location).(*pb.PBNotificationLocation),
	})
}

// UpdateItemCategoryAssignment updates an item's category using the two
// operations emitted by the AnyList web client. Both operations carry the
// complete item; the second operation carries the new category ID in
// originalValue (the captured wire contract does not use updatedValue).
func (c *Client) UpdateItemCategoryAssignment(ctx context.Context, listID string, item *pb.ListItem, oldCategoryID, newCategoryID, newCategoryName string) error {
	if strings.TrimSpace(listID) == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if item == nil || strings.TrimSpace(item.GetIdentifier()) == "" {
		return fmt.Errorf("item must include an identifier")
	}
	if item.GetListId() != "" && item.GetListId() != listID {
		return fmt.Errorf("item list ID %q does not match list ID %q", item.GetListId(), listID)
	}
	if strings.TrimSpace(oldCategoryID) == "" {
		oldCategoryID = item.GetCategoryMatchId()
	}
	if item.GetCategoryMatchId() != oldCategoryID {
		return fmt.Errorf("item category match %q does not match expected original category %q", item.GetCategoryMatchId(), oldCategoryID)
	}
	newCategoryID = strings.TrimSpace(newCategoryID)
	if newCategoryID == "" {
		return fmt.Errorf("new category ID must not be empty")
	}
	updated := proto.Clone(item).(*pb.ListItem)
	updated.ListId = listID
	updated.CategoryMatchId = newCategoryID
	if strings.TrimSpace(newCategoryName) != "" {
		updated.Category = newCategoryName
	}
	operations := &pb.PBListOperationList{Operations: []*pb.PBListOperation{
		{
			Metadata: &pb.PBOperationMetadata{
				OperationId: uuid.NewString(),
				HandlerId:   "update-list-item-category-assignment",
				UserId:      c.cfg.UserID,
			},
			ListId:     listID,
			ListItemId: item.GetIdentifier(),
			ListItem:   proto.Clone(item).(*pb.ListItem),
		},
		{
			Metadata: &pb.PBOperationMetadata{
				OperationId: uuid.NewString(),
				HandlerId:   "set-list-item-category-match-id",
				UserId:      c.cfg.UserID,
			},
			ListId:        listID,
			ListItemId:    item.GetIdentifier(),
			OriginalValue: newCategoryID,
			ListItem:      updated,
		},
	}}
	return c.sendListOperationsMultipart(ctx, operations)
}

// SaveItemPrice saves or clears an item's price. A clear is represented by a
// zero amount, matching the browser's save-item-price operation.
func (c *Client) SaveItemPrice(ctx context.Context, listID, itemID string, price *pb.PBItemPrice) error {
	if strings.TrimSpace(listID) == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("item ID must not be empty")
	}
	if price == nil {
		return fmt.Errorf("item price must not be nil")
	}
	return c.sendListOperationsMultipart(ctx, &pb.PBListOperationList{Operations: []*pb.PBListOperation{{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "save-item-price",
			UserId:      c.cfg.UserID,
		},
		ListId:     listID,
		ListItemId: itemID,
		ItemPrice:  proto.Clone(price).(*pb.PBItemPrice),
	}}})
}

// AddItemStoreID assigns an existing store to an item. The handler accepts
// the store identifier in updatedValue and preserves other store assignments.
func (c *Client) AddItemStoreID(ctx context.Context, listID, itemID, storeID string) error {
	return c.updateItemStoreID(ctx, "add-list-item-store-id", listID, itemID, storeID)
}

// RemoveItemStoreID removes one existing store assignment from an item.
func (c *Client) RemoveItemStoreID(ctx context.Context, listID, itemID, storeID string) error {
	return c.updateItemStoreID(ctx, "remove-list-item-store-id", listID, itemID, storeID)
}

func (c *Client) updateItemStoreID(ctx context.Context, handlerID, listID, itemID, storeID string) error {
	if strings.TrimSpace(listID) == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("item ID must not be empty")
	}
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return fmt.Errorf("store ID must not be empty")
	}
	return c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   handlerID,
			UserId:      c.cfg.UserID,
		},
		ListId:       listID,
		ListItemId:   itemID,
		UpdatedValue: storeID,
	})
}

// UpdateItemPackageSize updates the structured package-size metadata for an
// existing item. AnyList expects the complete current item on this operation;
// callers should therefore pass the live read-back item rather than a cache
// projection that may omit product metadata.
func (c *Client) UpdateItemPackageSize(ctx context.Context, listID string, item *pb.ListItem, packageSize *pb.PBItemPackageSize) error {
	if strings.TrimSpace(listID) == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if item == nil || strings.TrimSpace(item.GetIdentifier()) == "" {
		return fmt.Errorf("item must include an identifier")
	}
	if packageSize == nil {
		return fmt.Errorf("package size must not be nil")
	}
	updated := proto.Clone(item).(*pb.ListItem)
	updated.PackageSizePb = proto.Clone(packageSize).(*pb.PBItemPackageSize)
	return c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "set-list-item-package-size",
			UserId:      c.cfg.UserID,
		},
		ListId:     listID,
		ListItemId: item.GetIdentifier(),
		ListItem:   updated,
	})
}

// InspectPhotoFile validates a local photo without making a network request.
// The validation is deliberately shared by the CLI preview and upload paths
// so --dry-run cannot accept a file that the live path would reject.
func InspectPhotoFile(path string) (PhotoInfo, error) {
	_, info, err := readPhotoFile(path)
	return info, err
}

// UploadPhoto uploads a validated photo and returns the client-generated photo
// identifier. AnyList's upload endpoint does not provide a typed protobuf
// response; the identifier is the UUID used in the required UUID.jpg filename
// and is subsequently attached with SetItemPhotoID.
func (c *Client) UploadPhoto(ctx context.Context, path string) (string, PhotoInfo, error) {
	data, info, err := readPhotoFile(path)
	if err != nil {
		return "", PhotoInfo{}, err
	}
	photoID := strings.ReplaceAll(uuid.NewString(), "-", "")

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="photo"; filename="%s.jpg"`, photoID))
	header.Set("Content-Type", info.ContentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", PhotoInfo{}, fmt.Errorf("creating photo multipart part: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return "", PhotoInfo{}, fmt.Errorf("writing photo multipart part: %w", err)
	}
	if err := writer.WriteField("filename", photoID+".jpg"); err != nil {
		return "", PhotoInfo{}, fmt.Errorf("writing photo filename field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", PhotoInfo{}, fmt.Errorf("closing photo multipart body: %w", err)
	}

	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/photos/upload", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating photo upload request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return "", PhotoInfo{}, fmt.Errorf("uploading photo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", PhotoInfo{}, fmt.Errorf("photo upload failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(message)))
	}
	return photoID, info, nil
}

// SetItemPhotoID attaches an uploaded photo to a list item. The live client
// uses the typed list-operation protocol; callers must verify the item through
// a fresh GetUserData response before considering the mutation successful.
func (c *Client) SetItemPhotoID(ctx context.Context, listID, itemID, photoID string) error {
	listID = strings.TrimSpace(listID)
	itemID = strings.TrimSpace(itemID)
	photoID = strings.TrimSpace(photoID)
	if listID == "" || itemID == "" || photoID == "" {
		return fmt.Errorf("list ID, item ID, and photo ID must not be empty")
	}
	return c.sendListOperation(ctx, &pb.PBListOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "set-list-item-photo-id",
			UserId:      c.cfg.UserID,
		},
		ListId:       listID,
		ListItemId:   itemID,
		UpdatedValue: photoID,
	})
}

// AddStarterListItem adds an item to a starter list (user starters or
// favorites) using the typed starter-list protocol: a PBStarterListOperation
// with AnyList's bulk-add-list-items handler wrapped in a
// PBStarterListOperationList, posted as the binary "operations" multipart
// field to /data/starter-lists/update. The web client wraps the item in a
// partial StarterList (field 7); sending only the ListItem field is accepted
// by the transport but silently ignored by the server. The caller's item
// fields (name, quantity, details, category, and product metadata) are
// preserved verbatim; the client sets a generated stable identifier, the
// stable starter-list ID, and the user ID. The generated item ID is returned
// for exact fresh read-back. The endpoint can acknowledge an operation without
// applying it, so callers must read the starter list back before reporting
// success.
func (c *Client) AddStarterListItem(ctx context.Context, listID string, item *pb.ListItem) (string, error) {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return "", fmt.Errorf("list ID must not be empty")
	}
	if item == nil {
		return "", fmt.Errorf("item must not be nil")
	}
	if strings.TrimSpace(item.GetName()) == "" {
		return "", fmt.Errorf("item name must not be empty")
	}
	itemID := strings.ReplaceAll(uuid.NewString(), "-", "")
	updated := proto.Clone(item).(*pb.ListItem)
	updated.Identifier = itemID
	updated.ListId = listID
	updated.UserId = c.cfg.UserID
	// The web client records its neutral category in categoryMatchId. An empty
	// value is accepted by the transport but silently ignored by the server.
	if strings.TrimSpace(updated.GetCategoryMatchId()) == "" {
		updated.CategoryMatchId = "other"
	}
	return itemID, c.sendStarterListOperations(ctx, &pb.PBStarterListOperationList{Operations: []*pb.PBStarterListOperation{{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "bulk-add-list-items",
			UserId:      c.cfg.UserID,
		},
		ListId: listID,
		List: &pb.StarterList{
			Identifier: listID,
			Items:      []*pb.ListItem{updated},
		},
	}}})
}

// RemoveStarterListItem removes an item from a starter list (user starters or
// favorites) using the typed starter-list protocol and AnyList's exact
// "bulk-remove-list-items" handler. AnyList expects the complete current
// item payload; callers should pass an item from a fresh live read-back and
// re-read the starter list before reporting success.
func (c *Client) RemoveStarterListItem(ctx context.Context, listID string, item *pb.ListItem) error {
	listID = strings.TrimSpace(listID)
	if listID == "" {
		return fmt.Errorf("list ID must not be empty")
	}
	if item == nil || strings.TrimSpace(item.GetIdentifier()) == "" {
		return fmt.Errorf("item must include an identifier")
	}
	if item.GetListId() != "" && item.GetListId() != listID {
		return fmt.Errorf("item list ID %q does not match list ID %q", item.GetListId(), listID)
	}
	updated := proto.Clone(item).(*pb.ListItem)
	updated.ListId = listID
	if strings.TrimSpace(updated.GetCategoryMatchId()) == "" {
		updated.CategoryMatchId = "other"
	}
	return c.sendStarterListOperations(ctx, &pb.PBStarterListOperationList{Operations: []*pb.PBStarterListOperation{{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "bulk-remove-list-items",
			UserId:      c.cfg.UserID,
		},
		ListId: listID,
		List: &pb.StarterList{
			Identifier: listID,
			Items:      []*pb.ListItem{updated},
		},
	}}})
}

func readPhotoFile(path string) ([]byte, PhotoInfo, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, PhotoInfo{}, fmt.Errorf("photo file must not be empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, PhotoInfo{}, fmt.Errorf("opening photo file: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, PhotoInfo{}, fmt.Errorf("statting photo file: %w", err)
	}
	if !stat.Mode().IsRegular() {
		return nil, PhotoInfo{}, fmt.Errorf("photo path is not a regular file")
	}
	if stat.Size() > maxPhotoUploadBytes {
		return nil, PhotoInfo{}, fmt.Errorf("photo file exceeds %d bytes", maxPhotoUploadBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPhotoUploadBytes+1))
	if err != nil {
		return nil, PhotoInfo{}, fmt.Errorf("reading photo file: %w", err)
	}
	if int64(len(data)) > maxPhotoUploadBytes {
		return nil, PhotoInfo{}, fmt.Errorf("photo file exceeds %d bytes", maxPhotoUploadBytes)
	}
	contentType := detectPhotoContentType(path, data)
	if !allowedPhotoContentTypes[contentType] {
		return nil, PhotoInfo{}, fmt.Errorf("unsupported photo type %q; expected JPEG, PNG, GIF, BMP, TIFF, AVIF, or WebP", contentType)
	}
	return data, PhotoInfo{Size: int64(len(data)), ContentType: contentType}, nil
}

func detectPhotoContentType(path string, data []byte) string {
	contentType := http.DetectContentType(data)
	if allowedPhotoContentTypes[contentType] {
		return contentType
	}
	if len(data) >= 2 && data[0] == 'B' && data[1] == 'M' {
		return "image/bmp"
	}
	if len(data) >= 4 && ((data[0] == 'I' && data[1] == 'I' && data[2] == '*' && data[3] == 0) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0 && data[3] == '*')) {
		return "image/tiff"
	}
	if isAVIF(data) && strings.EqualFold(filepath.Ext(path), ".avif") {
		return "image/avif"
	}
	return contentType
}

func isAVIF(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	for offset := 8; offset+4 <= len(data) && offset < 64; offset += 4 {
		brand := string(data[offset : offset+4])
		if brand == "avif" || brand == "avis" {
			return true
		}
	}
	return false
}

// ProductLookup resolves a UPC/EAN through AnyList's product catalog. The
// response includes a ListItem carrying structured product metadata and, when
// available, a product thumbnail URL.
func (c *Client) ProductLookup(ctx context.Context, barcode string) (*pb.PBProductLookupResponse, error) {
	barcode = strings.TrimSpace(barcode)
	if barcode == "" {
		return nil, fmt.Errorf("barcode must not be empty")
	}
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			baseURL+"/data/product-lookup/"+url.PathEscape(barcode), nil)
		if err != nil {
			return nil, fmt.Errorf("creating product lookup request: %w", err)
		}
		c.setAuthHeaders(req)
		return req, nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("requesting product lookup: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading product lookup response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("product lookup failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out := &pb.PBProductLookupResponse{}
	if err := proto.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("decoding product lookup response: %w", err)
	}
	return out, nil
}

// UpdateItem updates fields of an existing list item from a full item value.
func (c *Client) UpdateItem(ctx context.Context, listID string, item *pb.ListItem) error {
	return c.UpdateItemFields(ctx, listID, item.GetIdentifier(), map[string]string{
		"name":              item.GetName(),
		"quantity":          item.GetQuantity(),
		"details":           item.GetDetails(),
		"category_match_id": item.GetCategoryMatchId(),
	})
}

// SaveRecipe creates or updates a recipe in the user's recipe data.
func (c *Client) SaveRecipe(ctx context.Context, recipeDataID string, recipe *pb.PBRecipe, fromWebImport bool) error {
	op := &pb.PBRecipeOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "save-recipe",
			UserId:      c.cfg.UserID,
		},
		RecipeDataId:             recipeDataID,
		Recipe:                   recipe,
		IsNewRecipeFromWebImport: fromWebImport,
	}
	return c.sendRecipeOperation(ctx, op)
}

// RemoveRecipe deletes a recipe from the user's recipe data.
func (c *Client) RemoveRecipe(ctx context.Context, recipeDataID string, recipe *pb.PBRecipe) error {
	op := &pb.PBRecipeOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "remove-recipe",
			UserId:      c.cfg.UserID,
		},
		RecipeDataId: recipeDataID,
		Recipe:       recipe,
		RecipeIds:    []string{recipe.GetIdentifier()},
	}
	return c.sendRecipeOperation(ctx, op)
}

// SaveRecipeCollection creates or updates a recipe collection.
func (c *Client) SaveRecipeCollection(ctx context.Context, recipeDataID string, collection *pb.PBRecipeCollection) error {
	op := &pb.PBRecipeOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "save-recipe-collection",
			UserId:      c.cfg.UserID,
		},
		RecipeDataId:        recipeDataID,
		RecipeCollection:    collection,
		RecipeCollectionIds: []string{collection.GetIdentifier()},
	}
	return c.sendRecipeOperation(ctx, op)
}

// RemoveRecipeCollection deletes a recipe collection.
func (c *Client) RemoveRecipeCollection(ctx context.Context, recipeDataID string, collection *pb.PBRecipeCollection) error {
	op := &pb.PBRecipeOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "remove-recipe-collection",
			UserId:      c.cfg.UserID,
		},
		RecipeDataId:        recipeDataID,
		RecipeCollection:    collection,
		RecipeCollectionIds: []string{collection.GetIdentifier()},
	}
	return c.sendRecipeOperation(ctx, op)
}

// RequestRecipeLink asks AnyList to share the current user's recipe data with
// the confirming email. The caller must fresh-read user data and verify the
// pending request before reporting success.
func (c *Client) RequestRecipeLink(ctx context.Context, request *pb.PBRecipeLinkRequest) (*pb.PBRecipeLinkRequestResponse, error) {
	if request == nil || strings.TrimSpace(request.GetIdentifier()) == "" || strings.TrimSpace(request.GetRequestingUserId()) == "" || strings.TrimSpace(request.GetConfirmingEmail()) == "" {
		return nil, fmt.Errorf("recipe link request requires identifier, requesting user ID, and confirming email")
	}
	dat, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshaling recipe link request: %w", err)
	}
	resp, err := c.postMultipartAuthed(ctx, "/data/user-recipe-data/request-recipe-link-v2", multipartFormField{Name: "link_request", Value: dat, Binary: true})
	if err != nil {
		return nil, fmt.Errorf("requesting recipe link: %w", err)
	}
	result := &pb.PBRecipeLinkRequestResponse{}
	if err := readProtoResponse(resp, result, "recipe link request"); err != nil {
		return nil, err
	}
	if result.GetStatusCode() >= http.StatusBadRequest {
		return nil, fmt.Errorf("recipe link request rejected (status %d): %s", result.GetStatusCode(), strings.TrimSpace(result.GetErrorMessage()))
	}
	return result, nil
}

// CancelRecipeLink cancels a pending recipe-link request. The caller must
// fresh-read user data and verify the request is absent afterward.
func (c *Client) CancelRecipeLink(ctx context.Context, request *pb.PBRecipeLinkRequest) (*pb.PBRecipeDataResponse, error) {
	if request == nil || strings.TrimSpace(request.GetIdentifier()) == "" {
		return nil, fmt.Errorf("recipe link cancellation requires a request identifier")
	}
	dat, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshaling recipe link cancellation: %w", err)
	}
	resp, err := c.postMultipartAuthed(ctx, "/data/user-recipe-data/cancel-recipe-link-request", multipartFormField{Name: "link_request", Value: dat, Binary: true})
	if err != nil {
		return nil, fmt.Errorf("cancelling recipe link: %w", err)
	}
	result := &pb.PBRecipeDataResponse{}
	if err := readProtoResponse(resp, result, "recipe link cancellation"); err != nil {
		return nil, err
	}
	return result, nil
}

// AcceptRecipeLink accepts a pending incoming recipe-link request. The caller
// must fresh-read user data and verify the requesting user is now linked.
func (c *Client) AcceptRecipeLink(ctx context.Context, requestID, userID string) (*pb.PBRecipeDataResponse, error) {
	requestID = strings.TrimSpace(requestID)
	userID = strings.TrimSpace(userID)
	if requestID == "" || userID == "" {
		return nil, fmt.Errorf("accepting a recipe link requires request ID and user ID")
	}
	resp, err := c.postMultipartAuthed(ctx, "/data/user-recipe-data/accept-recipe-link-request",
		multipartFormField{Name: "link_request_id", Value: []byte(requestID)},
		multipartFormField{Name: "user_id", Value: []byte(userID)})
	if err != nil {
		return nil, fmt.Errorf("accepting recipe link: %w", err)
	}
	result := &pb.PBRecipeDataResponse{}
	if err := readProtoResponse(resp, result, "recipe link acceptance"); err != nil {
		return nil, err
	}
	return result, nil
}

// UnlinkRecipes removes the current user's recipe link to userID. The caller
// must fresh-read user data and verify the linked user is absent afterward.
func (c *Client) UnlinkRecipes(ctx context.Context, userID string) (*pb.PBRecipeDataResponse, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("unlinking recipes requires a user ID")
	}
	resp, err := c.postMultipartAuthed(ctx, "/data/user-recipe-data/unlink-recipes", multipartFormField{Name: "user_id", Value: []byte(userID)})
	if err != nil {
		return nil, fmt.Errorf("unlinking recipes: %w", err)
	}
	result := &pb.PBRecipeDataResponse{}
	if err := readProtoResponse(resp, result, "recipe unlink"); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) sendRecipeOperation(ctx context.Context, op *pb.PBRecipeOperation) error {
	req := &pb.PBRecipeOperationList{Operations: []*pb.PBRecipeOperation{op}}
	dat, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling recipe operations: %w", err)
	}
	data := url.Values{}
	data.Set("operations", string(dat))

	resp, err := c.postFormAuthed(ctx, "/data/user-recipe-data/update", data)
	if err != nil {
		return fmt.Errorf("sending recipe operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("recipe operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

type multipartFormField struct {
	Name   string
	Value  []byte
	Binary bool
}

func (c *Client) postMultipartAuthed(ctx context.Context, path string, fields ...multipartFormField) (*http.Response, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, field := range fields {
		if field.Binary {
			header := make(textproto.MIMEHeader)
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, field.Name))
			header.Set("Content-Type", "application/octet-stream")
			part, err := writer.CreatePart(header)
			if err != nil {
				return nil, fmt.Errorf("creating multipart field %q: %w", field.Name, err)
			}
			if _, err := part.Write(field.Value); err != nil {
				return nil, fmt.Errorf("writing multipart field %q: %w", field.Name, err)
			}
			continue
		}
		if err := writer.WriteField(field.Name, string(field.Value)); err != nil {
			return nil, fmt.Errorf("writing multipart field %q: %w", field.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("closing multipart body: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	return c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
}

func readProtoResponse(resp *http.Response, message proto.Message, label string) error {
	if resp == nil {
		return fmt.Errorf("%s returned no response", label)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s failed (HTTP %d): %s", label, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading %s response: %w", label, err)
	}
	if len(body) == 0 {
		return fmt.Errorf("%s returned an empty protobuf response", label)
	}
	if err := proto.Unmarshal(body, message); err != nil {
		return fmt.Errorf("decoding %s response: %w", label, err)
	}
	return nil
}

// SaveCalendarEvent creates a meal-plan event with AnyList's typed calendar
// operation protocol. The caller must verify the event through a fresh
// GetUserData response before reporting success or updating local state.
//
// The calendar endpoint expects a PBCalendarOperationList in the multipart
// "operations" form field. AnyList uses updatedEvent for create, update, and
// delete operations.
func (c *Client) SaveCalendarEvent(ctx context.Context, calendarID string, event *pb.PBCalendarEvent) error {
	return c.sendCalendarEventOperation(ctx, "new-event", calendarID, event, nil)
}

// UpdateCalendarEvent updates an existing meal-plan event. If original is
// supplied, it is included for clients that have a fresh pre-write snapshot;
// the public operation contract still requires updatedEvent.
func (c *Client) UpdateCalendarEvent(ctx context.Context, calendarID string, event *pb.PBCalendarEvent, original ...*pb.PBCalendarEvent) error {
	var originalEvent *pb.PBCalendarEvent
	if len(original) > 0 {
		originalEvent = original[0]
	}
	return c.sendCalendarEventOperation(ctx, "update-event", calendarID, event, originalEvent)
}

// RemoveCalendarEvent deletes an existing meal-plan event. AnyList's typed
// calendar protocol carries the event in updatedEvent even for deletion.
func (c *Client) RemoveCalendarEvent(ctx context.Context, calendarID string, event *pb.PBCalendarEvent) error {
	return c.sendCalendarEventOperation(ctx, "delete-event", calendarID, event, nil)
}

func (c *Client) sendCalendarEventOperation(ctx context.Context, handlerID, calendarID string, event, original *pb.PBCalendarEvent) error {
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		return fmt.Errorf("calendar ID must not be empty")
	}
	if event == nil {
		return fmt.Errorf("calendar event must not be nil")
	}
	if strings.TrimSpace(event.GetIdentifier()) == "" {
		return fmt.Errorf("calendar event ID must not be empty")
	}
	updated := proto.Clone(event).(*pb.PBCalendarEvent)
	updated.CalendarId = calendarID
	op := &pb.PBCalendarOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   handlerID,
			UserId:      c.cfg.UserID,
		},
		CalendarId:   calendarID,
		UpdatedEvent: updated,
	}
	if original != nil {
		op.OriginalEvent = proto.Clone(original).(*pb.PBCalendarEvent)
		op.OriginalEvent.CalendarId = calendarID
	}
	return c.sendCalendarOperation(ctx, &pb.PBCalendarOperationList{Operations: []*pb.PBCalendarOperation{op}})
}

func (c *Client) sendCalendarOperation(ctx context.Context, operations *pb.PBCalendarOperationList) error {
	if operations == nil || len(operations.GetOperations()) == 0 {
		return fmt.Errorf("calendar operation list must not be empty")
	}
	dat, err := proto.Marshal(operations)
	if err != nil {
		return fmt.Errorf("marshaling calendar operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="operations"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating calendar operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing calendar operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing calendar operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/meal-planning-calendar/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating calendar operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending calendar operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("calendar operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// SaveListFolder creates or updates a list folder.
func (c *Client) SaveListFolder(ctx context.Context, listDataID string, folder *pb.PBListFolder, parentFolderID string) error {
	return c.SaveListFolderWithParents(ctx, listDataID, folder, "", parentFolderID)
}

// CreateListFolder sends AnyList's verified folder-creation operation. The
// caller must perform a fresh user-data read-back before reporting success.
func (c *Client) CreateListFolder(ctx context.Context, listDataID string, folder *pb.PBListFolder, parentFolderID string) error {
	if strings.TrimSpace(listDataID) == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil || strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder and ID must not be empty")
	}
	op := &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "create-new-folder",
			UserId:      c.cfg.UserID,
		},
		ListDataId:            listDataID,
		ListFolder:            folder,
		UpdatedParentFolderId: parentFolderID,
	}
	return c.sendListFolderOperation(ctx, op)
}

// RenameListFolder sends AnyList's verified set-folder-name operation. The
// caller must perform a fresh user-data read-back before reporting success.
func (c *Client) RenameListFolder(ctx context.Context, listDataID string, folder *pb.PBListFolder, newName string) error {
	if strings.TrimSpace(listDataID) == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil || strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder and ID must not be empty")
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return fmt.Errorf("new folder name must not be empty")
	}
	updated := proto.Clone(folder).(*pb.PBListFolder)
	updated.Name = newName
	return c.sendListFolderOperation(ctx, &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "set-folder-name",
			UserId:      c.cfg.UserID,
		},
		ListDataId: listDataID,
		ListFolder: updated,
	})
}

// MoveListFolderItems sends AnyList's verified parent-move operation. The
// caller must perform a fresh user-data read-back before reporting success.
func (c *Client) MoveListFolderItems(ctx context.Context, listDataID, folderID, originalParentFolderID, updatedParentFolderID string) error {
	if strings.TrimSpace(listDataID) == "" || strings.TrimSpace(folderID) == "" {
		return fmt.Errorf("list data ID and folder ID must not be empty")
	}
	if strings.TrimSpace(originalParentFolderID) == "" || strings.TrimSpace(updatedParentFolderID) == "" {
		return fmt.Errorf("original and updated parent folder IDs must not be empty")
	}
	if originalParentFolderID == updatedParentFolderID {
		return fmt.Errorf("original and updated parent folder IDs must differ")
	}
	return c.sendListFolderOperation(ctx, &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "move-folder-items",
			UserId:      c.cfg.UserID,
		},
		ListDataId:             listDataID,
		FolderItems:            []*pb.PBListFolderItem{{Identifier: folderID, ItemType: int32(pb.PBListFolderItem_FolderType)}},
		OriginalParentFolderId: originalParentFolderID,
		UpdatedParentFolderId:  updatedParentFolderID,
	})
}

// SaveListFolderWithParents updates a complete folder payload and, when a
// parent changes, carries both sides of the move in the typed operation.
func (c *Client) SaveListFolderWithParents(ctx context.Context, listDataID string, folder *pb.PBListFolder, originalParentFolderID, updatedParentFolderID string) error {
	if folder == nil || strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder and ID must not be empty")
	}
	op := &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "save-list-folder",
			UserId:      c.cfg.UserID,
		},
		ListDataId:             listDataID,
		ListFolder:             folder,
		OriginalParentFolderId: originalParentFolderID,
		UpdatedParentFolderId:  updatedParentFolderID,
	}
	return c.sendListFolderOperation(ctx, op)
}

// RemoveListFolder deletes a list folder.
func (c *Client) RemoveListFolder(ctx context.Context, listDataID string, folder *pb.PBListFolder) error {
	return c.RemoveListFolderFromParent(ctx, listDataID, folder, "")
}

// RemoveListFolderFromParent sends AnyList's folder-delete operation with the
// folder's current parent. The parent is part of the app's wire contract;
// callers that know it should provide it so the server can remove the
// membership and the folder in one operation.
func (c *Client) RemoveListFolderFromParent(ctx context.Context, listDataID string, folder *pb.PBListFolder, originalParentFolderID string) error {
	if strings.TrimSpace(listDataID) == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil || strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder and ID must not be empty")
	}
	op := &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "delete-folder-items",
			UserId:      c.cfg.UserID,
		},
		ListDataId: listDataID,
		FolderItems: []*pb.PBListFolderItem{{
			Identifier: folder.GetIdentifier(),
			ItemType:   int32(pb.PBListFolderItem_FolderType),
		}},
	}
	if parent := strings.TrimSpace(originalParentFolderID); parent != "" {
		op.OriginalParentFolderId = parent
	}
	return c.sendListFolderOperation(ctx, op)
}

// DeleteListFolderItems sends AnyList's observed delete-folder-items
// operation. It is transport-only: callers must verify a fresh folder
// read-back before reporting that anything was removed.
func (c *Client) DeleteListFolderItems(ctx context.Context, listDataID string, items ...*pb.PBListFolderItem) error {
	return c.DeleteListFolderItemsFromParent(ctx, listDataID, "", items...)
}

// DeleteListFolderItemsFromParent sends delete-folder-items with the
// original parent required by AnyList when removing memberships from a
// folder. It is useful for removing stale child references left behind by a
// deleted list or folder.
func (c *Client) DeleteListFolderItemsFromParent(ctx context.Context, listDataID, originalParentFolderID string, items ...*pb.PBListFolderItem) error {
	listDataID = strings.TrimSpace(listDataID)
	if listDataID == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if len(items) == 0 {
		return fmt.Errorf("at least one folder item must be supplied")
	}
	cloned := make([]*pb.PBListFolderItem, 0, len(items))
	for i, item := range items {
		if item == nil {
			return fmt.Errorf("folder item %d must not be nil", i)
		}
		if strings.TrimSpace(item.GetIdentifier()) == "" {
			return fmt.Errorf("folder item %d must have a non-empty identifier", i)
		}
		cloned = append(cloned, proto.Clone(item).(*pb.PBListFolderItem))
	}
	op := &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "delete-folder-items",
			UserId:      c.cfg.UserID,
		},
		ListDataId:  listDataID,
		FolderItems: cloned,
	}
	if parent := strings.TrimSpace(originalParentFolderID); parent != "" {
		op.OriginalParentFolderId = parent
	}
	return c.sendListFolderOperation(ctx, op)
}

func (c *Client) sendListFolderOperation(ctx context.Context, op *pb.PBListFolderOperation) error {
	req := &pb.PBListFolderOperationList{Operations: []*pb.PBListFolderOperation{op}}
	dat, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling list folder operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="operations"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating list folder operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing list folder operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing list folder operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/list-folders/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating list folder operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending list folder operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list folder operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// sendListFolderOperationBinary posts a list-folder operation using the
// multipart binary shape captured for folder metadata writes.
func (c *Client) sendListFolderOperationBinary(ctx context.Context, op *pb.PBListFolderOperation) error {
	return c.sendListFolderOperationWithPart(ctx, "operations", op)
}

func (c *Client) sendListFolderOperationWithPart(ctx context.Context, partName string, op *pb.PBListFolderOperation) error {
	req := &pb.PBListFolderOperationList{Operations: []*pb.PBListFolderOperation{op}}
	dat, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshaling list folder operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	partName = strings.TrimSpace(partName)
	if partName == "" {
		return fmt.Errorf("folder operations part name must not be empty")
	}
	header.Set("Content-Disposition", `form-data; name="`+partName+`"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating list folder operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing list folder operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing list folder operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/list-folders/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating list folder operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending list folder operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list folder operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func isSixDigitHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		switch {
		case value[i] >= '0' && value[i] <= '9':
		case value[i] >= 'a' && value[i] <= 'f':
		case value[i] >= 'A' && value[i] <= 'F':
		default:
			return false
		}
	}
	return true
}

func cloneFolderWithSettings(folder *pb.PBListFolder, mutate func(*pb.PBListFolderSettings)) *pb.PBListFolder {
	cloned := proto.Clone(folder).(*pb.PBListFolder)
	if cloned.FolderSettings == nil {
		cloned.FolderSettings = &pb.PBListFolderSettings{}
	}
	mutate(cloned.FolderSettings)
	return cloned
}

// SetListFolderHexColor changes only the hex color of an existing folder's
// settings. The captured handler is set-folder-hex-color; callers must still
// verify persistence with a fresh folder read-back.
func (c *Client) SetListFolderHexColor(ctx context.Context, listDataID string, folder *pb.PBListFolder, hexColor string) error {
	listDataID = strings.TrimSpace(listDataID)
	if listDataID == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil {
		return fmt.Errorf("list folder must not be nil")
	}
	if strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder ID must not be empty")
	}
	if !isSixDigitHexColor(hexColor) {
		return fmt.Errorf("folder hex color %q must be a #RRGGBB value", hexColor)
	}
	return c.sendListFolderOperationBinary(ctx, &pb.PBListFolderOperation{
		Metadata:   &pb.PBOperationMetadata{OperationId: uuid.NewString(), HandlerId: "set-folder-hex-color", UserId: c.cfg.UserID},
		ListDataId: listDataID,
		ListFolder: cloneFolderWithSettings(folder, func(settings *pb.PBListFolderSettings) { settings.FolderHexColor = hexColor }),
	})
}

// SetListFolderSortPosition changes only the folder sort position. Valid
// values are AnyList's AfterLists (0), BeforeLists (1), and WithLists (2).
// Callers must verify persistence with a fresh folder read-back.
func (c *Client) SetListFolderSortPosition(ctx context.Context, listDataID string, folder *pb.PBListFolder, position pb.PBListFolderSettings_FolderSortPosition) error {
	listDataID = strings.TrimSpace(listDataID)
	if listDataID == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil {
		return fmt.Errorf("list folder must not be nil")
	}
	if strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder ID must not be empty")
	}
	if _, ok := pb.PBListFolderSettings_FolderSortPosition_name[int32(position)]; !ok {
		return fmt.Errorf("folder sort position %d must be 0 (AfterLists), 1 (BeforeLists), or 2 (WithLists)", position)
	}
	return c.sendListFolderOperationBinary(ctx, &pb.PBListFolderOperation{
		Metadata:   &pb.PBOperationMetadata{OperationId: uuid.NewString(), HandlerId: "set-folder-sort-position", UserId: c.cfg.UserID},
		ListDataId: listDataID,
		ListFolder: cloneFolderWithSettings(folder, func(settings *pb.PBListFolderSettings) { settings.FolderSortPosition = int32(position) }),
	})
}

// SetOrderedFolderItems constructs the web client's exact
// set-ordered-folder-items operation for a complete child order. AnyList's
// app sends the ordered child entries plus the folder ID in the protobuf
// field named originalParentFolderId (despite that misleading field name); it
// does not send a full PBListFolder or an updated parent in this operation.
// Callers must fresh-read and verify the order before reporting success.
func (c *Client) SetOrderedFolderItems(ctx context.Context, listDataID string, folder *pb.PBListFolder, items []*pb.PBListFolderItem) error {
	listDataID = strings.TrimSpace(listDataID)
	if listDataID == "" {
		return fmt.Errorf("list data ID must not be empty")
	}
	if folder == nil {
		return fmt.Errorf("list folder must not be nil")
	}
	if strings.TrimSpace(folder.GetIdentifier()) == "" {
		return fmt.Errorf("list folder ID must not be empty")
	}
	if len(items) == 0 {
		return fmt.Errorf("at least one ordered folder item must be supplied")
	}
	cloned := make([]*pb.PBListFolderItem, 0, len(items))
	for i, item := range items {
		if item == nil {
			return fmt.Errorf("folder item %d must not be nil", i)
		}
		if strings.TrimSpace(item.GetIdentifier()) == "" {
			return fmt.Errorf("folder item %d must have a non-empty identifier", i)
		}
		cloned = append(cloned, proto.Clone(item).(*pb.PBListFolderItem))
	}
	return c.sendListFolderOperationBinary(ctx, &pb.PBListFolderOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   "set-ordered-folder-items",
			UserId:      c.cfg.UserID,
		},
		ListDataId:             listDataID,
		FolderItems:            cloned,
		OriginalParentFolderId: folder.GetIdentifier(),
	})
}

func cloneFolderWithItems(folder *pb.PBListFolder, items []*pb.PBListFolderItem) *pb.PBListFolder {
	cloned := proto.Clone(folder).(*pb.PBListFolder)
	cloned.Items = items
	return cloned
}

// UpdateListSettings sends one typed list-settings operation to the
// list-settings update endpoint. The handler ID must be supplied by the
// caller (for example, from a captured wire contract); the client never
// invents, defaults, or substitutes a handler. The complete caller payload
// is carried in the operation's updatedSettings field, including its
// non-empty ListId.
//
// An HTTP 200 response is only a transport acknowledgment; it is not proof
// that the settings persisted. Callers must verify the write with a fresh
// settings read-back before reporting success.
func (c *Client) UpdateListSettings(ctx context.Context, handlerID string, settings *pb.PBListSettings) error {
	handlerID = strings.TrimSpace(handlerID)
	if handlerID == "" {
		return fmt.Errorf("handler ID must not be empty")
	}
	if settings == nil {
		return fmt.Errorf("list settings must not be nil")
	}
	if strings.TrimSpace(settings.GetListId()) == "" {
		return fmt.Errorf("list settings must include a non-empty list ID")
	}
	return c.sendListSettingsOperation(ctx, handlerID, settings)
}

func (c *Client) sendListSettingsOperation(ctx context.Context, handlerID string, settings *pb.PBListSettings) error {
	op := &pb.PBListSettingsOperation{
		Metadata: &pb.PBOperationMetadata{
			OperationId: uuid.NewString(),
			HandlerId:   handlerID,
			UserId:      c.cfg.UserID,
		},
		UpdatedSettings: proto.Clone(settings).(*pb.PBListSettings),
	}
	return c.sendListSettingsOperations(ctx, &pb.PBListSettingsOperationList{Operations: []*pb.PBListSettingsOperation{op}})
}

func (c *Client) sendListSettingsOperations(ctx context.Context, operations *pb.PBListSettingsOperationList) error {
	if operations == nil || len(operations.GetOperations()) == 0 {
		return fmt.Errorf("list settings operation list must not be empty")
	}
	dat, err := proto.Marshal(operations)
	if err != nil {
		return fmt.Errorf("marshaling list settings operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="operations"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating list settings operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing list settings operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing list settings operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/list-settings/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating list settings operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending list settings operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list settings operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// sendStarterListOperations posts a PBStarterListOperationList as the binary
// "operations" multipart field to /data/starter-lists/update. Starter
// mutations never reuse the shopping-list route or a form-encoded fallback.
func (c *Client) sendStarterListOperations(ctx context.Context, ops *pb.PBStarterListOperationList) error {
	if ops == nil || len(ops.GetOperations()) == 0 {
		return fmt.Errorf("starter list operation list must not be empty")
	}
	dat, err := proto.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshaling starter list operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="operations"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating starter list operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing starter list operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing starter list operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/starter-lists/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating starter list operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending starter list operation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("starter list operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// sendListOperation sends a single list operation as a protobuf form POST.
func (c *Client) sendListOperation(ctx context.Context, op *pb.PBListOperation) error {
	req := &pb.PBListOperationList{Operations: []*pb.PBListOperation{op}}
	return c.sendListOperations(ctx, req)
}

// sendListOperations sends a batch of list operations.
func (c *Client) sendListOperations(ctx context.Context, ops *pb.PBListOperationList) error {
	dat, err := proto.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshaling list operations: %w", err)
	}
	data := url.Values{}
	data.Set("operations", string(dat))

	resp, err := c.postFormAuthed(ctx, "/data/shopping-lists/update", data)
	if err != nil {
		return fmt.Errorf("sending list operations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ShareListInvite sends a shopping-list share invitation using the wire
// shape captured from AnyList's own web client: POST
// /data/shopping-lists/share-list with a multipart form field named
// "operation" carrying the protobuf payload as an ordinary string part.
// The checked-in generated proto has no request message for this route, so
// the outer request fields are encoded by shareListInvitePayload below.
func (c *Client) ShareListInvite(ctx context.Context, listID, targetEmail string) (*pb.PBShareListOperationResponse, error) {
	if listID == "" || listID != strings.TrimSpace(listID) {
		return nil, fmt.Errorf("list ID must be a non-empty exact value, got %q", listID)
	}
	if targetEmail == "" || targetEmail != strings.TrimSpace(targetEmail) {
		return nil, fmt.Errorf("target email must be a non-empty exact value, got %q", targetEmail)
	}
	parsedEmail, err := mail.ParseAddress(targetEmail)
	if err != nil || parsedEmail.Address != targetEmail {
		return nil, fmt.Errorf("invalid target email %q", targetEmail)
	}
	if c.cfg.UserID == "" || c.cfg.UserID != strings.TrimSpace(c.cfg.UserID) {
		return nil, fmt.Errorf("authenticated user ID is not set; run 'anylist-pp-cli auth login' first")
	}
	payload, err := shareListInvitePayload(c.cfg.UserID, listID, targetEmail)
	if err != nil {
		return nil, fmt.Errorf("building share-list invitation payload: %w", err)
	}
	resp, err := c.postMultipartAuthed(ctx, "/data/shopping-lists/share-list", multipartFormField{Name: "operation", Value: payload})
	if err != nil {
		return nil, fmt.Errorf("sending share-list invitation: %w", err)
	}
	out := &pb.PBShareListOperationResponse{}
	if err := readProtoResponse(resp, out, "share-list invitation"); err != nil {
		return nil, err
	}
	if out.GetStatusCode() != 0 {
		return nil, fmt.Errorf("share-list invitation failed (statusCode %d): %s: %s", out.GetStatusCode(), out.GetErrorTitle(), out.GetErrorMessage())
	}
	if out.GetErrorTitle() != "" || out.GetErrorMessage() != "" {
		return nil, fmt.Errorf("share-list invitation returned an error: %s: %s", out.GetErrorTitle(), out.GetErrorMessage())
	}
	return out, nil
}

// shareListInvitePayload encodes the captured share-shopping-list request.
// Its outer message has field 1 PBOperationMetadata, field 2 list ID, and
// field 4 target email; field 3 was absent from the capture and stays absent.
func shareListInvitePayload(userID, listID, targetEmail string) ([]byte, error) {
	metadata := &pb.PBOperationMetadata{OperationId: uuid.NewString(), HandlerId: "share-shopping-list", UserId: userID}
	metaBytes, err := proto.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshaling operation metadata: %w", err)
	}
	var buf bytes.Buffer
	writeShareListInviteLengthDelimited(&buf, 1, metaBytes)
	writeShareListInviteLengthDelimited(&buf, 2, []byte(listID))
	writeShareListInviteLengthDelimited(&buf, 4, []byte(targetEmail))
	return buf.Bytes(), nil
}

func writeShareListInviteLengthDelimited(buf *bytes.Buffer, fieldNum int, value []byte) {
	buf.Write(shareListInviteVarint(uint64(fieldNum<<3) | 2))
	buf.Write(shareListInviteVarint(uint64(len(value))))
	buf.Write(value)
}

func shareListInviteVarint(value uint64) []byte {
	out := make([]byte, 0, 10)
	for value >= 0x80 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

func (c *Client) sendListOperationsMultipart(ctx context.Context, ops *pb.PBListOperationList) error {
	if ops == nil || len(ops.GetOperations()) == 0 {
		return fmt.Errorf("list operation list must not be empty")
	}
	dat, err := proto.Marshal(ops)
	if err != nil {
		return fmt.Errorf("marshaling list operations: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="operations"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(header)
	if err != nil {
		return fmt.Errorf("creating list operations part: %w", err)
	}
	if _, err := part.Write(dat); err != nil {
		return fmt.Errorf("writing list operations part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("closing list operations form: %w", err)
	}
	bodyBytes := body.Bytes()
	contentType := writer.FormDataContentType()
	resp, err := c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/data/shopping-lists/update", bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("creating list operation request: %w", err)
		}
		c.setAuthHeaders(req)
		req.Header.Set("Content-Type", contentType)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return fmt.Errorf("sending list operations: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list operation failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return nil
}

// postForm sends an unauthenticated form POST (used for login/refresh).
func (c *Client) postForm(ctx context.Context, path string, data url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+path, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	resp, err := c.doHTTP(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rateErr := &RateLimitError{RetryAfter: resp.Header.Get("Retry-After")}
		resp.Body.Close()
		return nil, rateErr
	}
	return resp, nil
}

// postFormAuthed sends an authenticated form POST and retries once on 401.
func (c *Client) postFormAuthed(ctx context.Context, path string, data url.Values) (*http.Response, error) {
	body := []byte(data.Encode())
	return c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			baseURL+path, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		c.setAuthHeaders(req)
		return req, nil
	}, body)
}

func (c *Client) setCommonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-AnyLeaf-API-Version", apiVersion)
	if c.cfg.ClientIdentifier != "" {
		req.Header.Set("X-AnyLeaf-Client-Identifier", c.cfg.ClientIdentifier)
	}
}

func (c *Client) setAuthHeaders(req *http.Request) {
	c.setCommonHeaders(req)
	if c.cfg.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
	}
}

// postRaw sends an authenticated POST with no body (for user-data/get).
// It retries once on HTTP 401 by refreshing the token.
func (c *Client) postRaw(ctx context.Context, path string) (*http.Response, error) {
	return c.doWithRetry(ctx, func(b []byte) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-AnyLeaf-API-Version", apiVersion)
		if c.cfg.ClientIdentifier != "" {
			req.Header.Set("X-AnyLeaf-Client-Identifier", c.cfg.ClientIdentifier)
		}
		if c.cfg.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.AccessToken)
		}
		return req, nil
	}, nil)
}
