package onedrive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
)

type Client struct {
	graph      *msgraphsdk.GraphServiceClient
	driveID    string
	download   *http.Client
	retryDelay time.Duration
}

func New(ctx context.Context, cfg config.Config, stderr io.Writer) (*Client, error) {
	credential, err := newCredential(cfg, stderr)
	if err != nil {
		return nil, err
	}
	graph, err := msgraphsdk.NewGraphServiceClientWithCredentials(credential, []string{fileScope})
	if err != nil {
		return nil, safeFailure("initialize Microsoft Graph SDK", err)
	}
	return newClient(ctx, graph, downloadClient())
}

func newClient(ctx context.Context, graph *msgraphsdk.GraphServiceClient, download *http.Client) (*Client, error) {
	drive, err := graph.Me().Drive().Get(ctx, nil)
	if err != nil {
		return nil, safeFailure("read personal OneDrive", err)
	}
	if drive == nil || text(drive.GetId()) == "" {
		return nil, errors.New("Microsoft Graph returned no OneDrive ID")
	}
	if text(drive.GetDriveType()) != "personal" {
		return nil, errors.New("this application requires a personal OneDrive, not a business or document-library drive")
	}
	return &Client{graph: graph, driveID: *drive.GetId(), download: download, retryDelay: time.Second}, nil
}

func (c *Client) File(ctx context.Context, id string) (model.SourceFile, error) {
	if id == "" {
		return model.SourceFile{}, errors.New("OneDrive item ID must not be empty")
	}
	item, err := c.graph.Drives().ByDriveId(c.driveID).Items().ByDriveItemId(id).Get(ctx, nil)
	if err != nil {
		return model.SourceFile{}, safeFailure("read selected OneDrive item by ID", err)
	}
	if item == nil || text(item.GetId()) != id {
		return model.SourceFile{}, errors.New("Microsoft Graph returned a missing or different selected item ID")
	}
	if item.GetDeleted() != nil {
		return model.SourceFile{}, errors.New("selected OneDrive item is deleted")
	}
	kind := itemType(item)
	if kind != "file" || item.GetRoot() != nil {
		return model.SourceFile{}, fmt.Errorf("selected OneDrive item is %s; only regular owned-drive files can be copied", kind)
	}
	if parent := item.GetParentReference(); parent != nil && text(parent.GetDriveId()) != "" && text(parent.GetDriveId()) != c.driveID {
		return model.SourceFile{}, errors.New("selected OneDrive file belongs to another drive")
	}
	if text(item.GetName()) == "" {
		return model.SourceFile{}, errors.New("selected OneDrive file has no name")
	}
	if item.GetSize() != nil && *item.GetSize() < 0 {
		return model.SourceFile{}, errors.New("selected OneDrive file has an invalid negative size")
	}
	mime := text(item.GetFile().GetMimeType())
	if mime == "" {
		mime = "application/octet-stream"
	}
	downloadURL := additionalString(item.GetAdditionalData()["@microsoft.graph.downloadUrl"])
	if err := validateDownloadURL(downloadURL); err != nil {
		return model.SourceFile{}, err
	}
	return model.SourceFile{ID: id, Name: *item.GetName(), MIMEType: mime, Size: item.GetSize(), DownloadURL: downloadURL}, nil
}

func additionalString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case *string:
		return text(value)
	default:
		return ""
	}
}

func itemType(item models.DriveItemable) string {
	switch {
	case item.GetRemoteItem() != nil:
		return "remote"
	case item.GetPackageEscaped() != nil:
		return "package"
	case item.GetFolder() != nil:
		return "folder"
	case item.GetFile() != nil:
		return "file"
	default:
		return "unknown"
	}
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
