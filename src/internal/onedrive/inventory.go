package onedrive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
	graphdrives "github.com/microsoftgraph/msgraph-sdk-go/drives"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

func (c *Client) Inventory(ctx context.Context) ([]model.Item, error) {
	for attempt := 0; attempt < 2; attempt++ {
		items, err := c.inventory(ctx)
		if err == nil {
			return items, nil
		}
		var status interface{ GetStatusCode() int }
		if !errors.As(err, &status) || status.GetStatusCode() != http.StatusGone || attempt == 1 {
			return nil, err
		}
	}
	return nil, errors.New("OneDrive inventory could not resynchronize")
}

func (c *Client) inventory(ctx context.Context) ([]model.Item, error) {
	root, err := c.graph.Drives().ByDriveId(c.driveID).Root().Get(ctx, nil)
	if err != nil {
		return nil, safeFailure("read OneDrive root", err)
	}
	if root == nil || text(root.GetId()) == "" || root.GetRoot() == nil || root.GetDeleted() != nil {
		return nil, errors.New("Microsoft Graph returned an invalid OneDrive root")
	}
	rootID := *root.GetId()
	baseURL := strings.TrimRight(c.graph.GetAdapter().GetBaseUrl(), "/")
	initialURL := baseURL + "/drives/" + url.PathEscape(c.driveID) + "/root/delta"
	builder := graphdrives.NewItemItemsItemDeltaRequestBuilder(initialURL, c.graph.GetAdapter())
	nextURL := initialURL
	seenPages := make(map[string]bool)
	byID := make(map[string]models.DriveItemable)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if seenPages[nextURL] {
			return nil, errors.New("OneDrive delta pagination repeated a page; inventory is incomplete")
		}
		seenPages[nextURL] = true
		page, err := builder.WithUrl(nextURL).GetAsDeltaGetResponse(ctx, nil)
		if err != nil {
			return nil, safeFailure("enumerate complete OneDrive delta inventory", err)
		}
		if page == nil {
			return nil, errors.New("Microsoft Graph returned an empty delta response; inventory is incomplete")
		}
		for _, item := range page.GetValue() {
			if item == nil || text(item.GetId()) == "" {
				return nil, errors.New("OneDrive delta contains an item without an ID")
			}
			id := *item.GetId()
			if item.GetDeleted() != nil {
				if id == rootID {
					return nil, errors.New("OneDrive root was deleted during inventory")
				}
				delete(byID, id)
				continue
			}
			byID[id] = item
		}
		nextURL = text(page.GetOdataNextLink())
		if nextURL == "" {
			if text(page.GetOdataDeltaLink()) == "" {
				return nil, errors.New("OneDrive delta response has no continuation or completion link; inventory is incomplete")
			}
			break
		}
		if !validGraphLink(baseURL, nextURL) {
			return nil, errors.New("Microsoft Graph returned an invalid delta continuation URL")
		}
	}
	return reconstructPaths(ctx, c.driveID, rootID, byID)
}

func validGraphLink(base, link string) bool {
	expected, err := url.Parse(base)
	if err != nil {
		return false
	}
	actual, err := url.Parse(link)
	return err == nil && actual.Scheme == "https" && actual.Host == expected.Host && actual.User == nil && actual.Fragment == ""
}

func reconstructPaths(ctx context.Context, driveID, rootID string, byID map[string]models.DriveItemable) ([]model.Item, error) {
	if root, ok := byID[rootID]; ok {
		if root.GetRoot() == nil {
			return nil, errors.New("OneDrive delta root is missing its root facet")
		}
		delete(byID, rootID)
	}
	paths := map[string]string{rootID: ""}
	result := make([]model.Item, 0, len(byID))
	for id := range byID {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chain := []string{}
		visiting := make(map[string]bool)
		current := id
		for {
			if _, resolved := paths[current]; resolved {
				break
			}
			if visiting[current] {
				return nil, fmt.Errorf("OneDrive inventory has a parent cycle at item %q", current)
			}
			item, exists := byID[current]
			if !exists {
				return nil, fmt.Errorf("OneDrive inventory has an orphan: missing parent %q", current)
			}
			if item.GetRoot() != nil {
				return nil, fmt.Errorf("OneDrive inventory has an unexpected root at item %q", current)
			}
			name := text(item.GetName())
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
				return nil, fmt.Errorf("OneDrive inventory item %q has a missing or invalid name", current)
			}
			parent := item.GetParentReference()
			if parent == nil || text(parent.GetId()) == "" {
				return nil, fmt.Errorf("OneDrive inventory item %q has no parent ID", current)
			}
			if text(parent.GetDriveId()) != "" && *parent.GetDriveId() != driveID {
				return nil, fmt.Errorf("OneDrive inventory item %q has a parent in another drive", current)
			}
			visiting[current] = true
			chain = append(chain, current)
			current = *parent.GetId()
		}
		for i := len(chain) - 1; i >= 0; i-- {
			item := byID[chain[i]]
			paths[chain[i]] = paths[*item.GetParentReference().GetId()] + "/" + *item.GetName()
		}
		item := byID[id]
		result = append(result, model.Item{ID: id, Path: paths[id], Name: *item.GetName(), Type: itemType(item), Size: item.GetSize()})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == result[j].Path {
			return result[i].ID < result[j].ID
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}
