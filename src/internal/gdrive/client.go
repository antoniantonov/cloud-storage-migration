// Package gdrive copies binary files into a personal My Drive root folder using
// Desktop OAuth consent and client-keyed, private token caches. It never deletes
// or overwrites Drive objects, and does not support service accounts or Shared Drives.
package gdrive

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	folderName    = "test-migration"
	folderMIME    = "application/vnd.google-apps.folder"
	uploadChunk   = 5 * 1024 * 1024
	verifyFields  = "id,name,mimeType,parents,trashed,driveId,size,md5Checksum"
	folderQuery   = "trashed = false and 'root' in parents and mimeType = 'application/vnd.google-apps.folder' and name = 'test-migration'"
	listFields    = "nextPageToken,incompleteSearch,files(id,name,mimeType,trashed,driveId)"
	uploadFields  = "id,size,md5Checksum"
	chunkDeadline = 32 * time.Second
)

type Client struct {
	service *drive.Service
	retry   retryPolicy
}

// New reuses or obtains delegated credentials. The caller must hold the shared
// AUTH_DIR process lock for the client's lifetime.
func New(ctx context.Context, cfg config.Config, stderr io.Writer) (*Client, error) {
	httpClient, err := authenticatedClient(ctx, cfg, stderr)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = chunkRetryAfterTransport{base: httpClient.Transport, sleep: sleepContext}
	service, err := drive.NewService(ctx, option.WithHTTPClient(httpClient),
		option.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		return nil, driveError("initialize client", err)
	}
	return &Client{service: service, retry: defaultRetryPolicy()}, nil
}

func (c *Client) list(ctx context.Context, query string) ([]*drive.File, error) {
	var files []*drive.File
	page := ""
	seen := map[string]bool{}
	for {
		response, err := retryCall(ctx, c.retry, func() (*drive.FileList, error) {
			return c.service.Files.List().Q(query).Spaces("drive").Corpora("user").
				IncludeItemsFromAllDrives(false).SupportsAllDrives(false).PageSize(1000).
				PageToken(page).Fields(listFields).Context(ctx).Do()
		})
		if err != nil {
			return nil, driveError("list destination", err)
		}
		if response == nil || response.IncompleteSearch {
			return nil, diagnostic("Google Drive destination search is incomplete; no objects were created")
		}
		for _, file := range response.Files {
			if file == nil || file.Id == "" {
				return nil, diagnostic("Google Drive destination search returned invalid file metadata")
			}
			files = append(files, file)
		}
		if response.NextPageToken == "" {
			return files, nil
		}
		if seen[response.NextPageToken] {
			return nil, diagnostic("Google Drive destination pagination repeated a page token; search is incomplete")
		}
		page = response.NextPageToken
		seen[page] = true
	}
}

func (c *Client) EnsureFolder(ctx context.Context) (string, error) {
	files, err := c.list(ctx, folderQuery)
	if err != nil {
		return "", err
	}
	ids := map[string]struct{}{}
	for _, file := range files {
		if file.Name == folderName && file.MimeType == folderMIME && !file.Trashed && file.DriveId == "" {
			ids[file.Id] = struct{}{}
		}
	}
	if len(ids) > 1 {
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		return "", diagnostic(fmt.Sprintf("multiple non-trashed My Drive root folders named %q exist (IDs: %q); resolve the ambiguity before retrying", folderName, sorted))
	}
	for id := range ids {
		return id, nil
	}
	id, err := c.generateID(ctx)
	if err != nil {
		return "", err
	}
	file, err := retryCall(ctx, c.retry, func() (*drive.File, error) {
		created, err := c.service.Files.Create(&drive.File{
			Id: id, Name: folderName, MimeType: folderMIME, Parents: []string{"root"},
		}).Fields("id").Context(ctx).Do()
		if isConflict(err) {
			return c.verifyFolder(ctx, id)
		}
		return created, err
	})
	if err != nil {
		return "", driveError("create destination folder", err)
	}
	if file == nil || file.Id != id {
		return "", diagnostic("Google Drive folder creation returned an unexpected ID; completion is uncertain")
	}
	return file.Id, nil
}

func queryLiteral(value string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(value) + "'"
}

func (c *Client) ExistingNames(ctx context.Context, folderID string) (map[string]struct{}, error) {
	if folderID == "" {
		return nil, diagnostic("Google destination folder ID is empty")
	}
	files, err := c.list(ctx, "trashed = false and "+queryLiteral(folderID)+" in parents")
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(files))
	for _, file := range files {
		if !file.Trashed && file.DriveId == "" {
			names[file.Name] = struct{}{}
		}
	}
	return names, nil
}

func (c *Client) generateID(ctx context.Context) (string, error) {
	result, err := retryCall(ctx, c.retry, func() (*drive.GeneratedIds, error) {
		return c.service.Files.GenerateIds().Count(1).Space("drive").Type("files").Fields("ids").Context(ctx).Do()
	})
	if err != nil {
		return "", driveError("reserve object ID", err)
	}
	if result == nil || len(result.Ids) != 1 || result.Ids[0] == "" {
		return "", diagnostic("Google Drive did not return exactly one reserved object ID")
	}
	return result.Ids[0], nil
}

func isConflict(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 409
}

func (c *Client) get(ctx context.Context, id string) (*drive.File, error) {
	file, err := retryCall(ctx, c.retry, func() (*drive.File, error) {
		return c.service.Files.Get(id).Fields(verifyFields).Context(ctx).Do()
	})
	if err != nil {
		return nil, driveError("verify existing reserved object", err)
	}
	if file == nil || file.Id == "" {
		return nil, diagnostic("Google Drive returned no metadata for the reserved object")
	}
	return file, nil
}

func (c *Client) verifyFolder(ctx context.Context, id string) (*drive.File, error) {
	file, err := c.get(ctx, id)
	if err != nil {
		return nil, err
	}
	root, err := c.get(ctx, "root")
	if err != nil {
		return nil, err
	}
	if file.Id != id || file.Name != folderName || file.MimeType != folderMIME ||
		file.Trashed || file.DriveId != "" || len(file.Parents) != 1 || file.Parents[0] != root.Id {
		return nil, diagnostic("Google Drive reserved folder ID already exists but its metadata does not match; nothing was overwritten")
	}
	return file, nil
}

func binaryMIME(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || strings.HasPrefix(mediaType, "application/vnd.google-apps.") {
		return "application/octet-stream"
	}
	return mediaType
}

func (c *Client) Upload(ctx context.Context, folderID string, source model.SourceFile, local *os.File, localMD5 string) (model.UploadedFile, error) {
	if folderID == "" || source.Name == "" || local == nil {
		return model.UploadedFile{}, diagnostic("Google upload requires a destination folder, source filename, and open local file")
	}
	checksum, err := hex.DecodeString(localMD5)
	if err != nil || len(checksum) != 16 {
		return model.UploadedFile{}, diagnostic("Google upload requires the completed local file's MD5 checksum")
	}
	info, err := local.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return model.UploadedFile{}, diagnostic("cannot inspect the regular local upload file")
	}
	id, err := c.generateID(ctx)
	if err != nil {
		return model.UploadedFile{}, err
	}
	contentType := binaryMIME(source.MIMEType)
	// A bounded whole-create retry also handles initiation failures and Drive's
	// rate-limit 403s. Each attempt keeps the same reserved ID; SDK chunk retries
	// remain independently limited to chunkDeadline.
	policy := c.retry
	if policy.attempts > 3 {
		policy.attempts = 3
	}
	file, err := retryCall(ctx, policy, func() (*drive.File, error) {
		if _, err := local.Seek(0, io.SeekStart); err != nil {
			return nil, diagnostic("cannot rewind the local upload file")
		}
		created, err := c.service.Files.Create(&drive.File{
			Id: id, Name: source.Name, MimeType: contentType, Parents: []string{folderID},
		}).Media(local, googleapi.ContentType(contentType), googleapi.ChunkSize(uploadChunk),
			googleapi.ChunkRetryDeadline(chunkDeadline), googleapi.ChunkTransferTimeout(30*time.Second)).
			Fields(uploadFields).Context(ctx).Do()
		if isConflict(err) {
			existing, err := c.get(ctx, id)
			if err != nil {
				return nil, err
			}
			if existing.Id != id || existing.Name != source.Name || existing.MimeType != contentType ||
				existing.Trashed || existing.DriveId != "" || len(existing.Parents) != 1 ||
				existing.Parents[0] != folderID || existing.Size != info.Size() ||
				!strings.EqualFold(existing.Md5Checksum, localMD5) {
				return nil, diagnostic("Google Drive reserved file ID already exists but its metadata/checksum does not match; upload completion is uncertain and nothing was overwritten")
			}
			return existing, nil
		}
		return created, err
	})
	if err != nil {
		return model.UploadedFile{}, driveError("upload", err)
	}
	if file == nil || file.Id != id {
		return model.UploadedFile{}, diagnostic("Google Drive upload returned an unexpected ID; completion is uncertain")
	}
	result := model.UploadedFile{ID: file.Id, MD5: file.Md5Checksum}
	// The SDK cannot distinguish an absent size from zero. Preserve unknown,
	// except when the empty-content checksum independently confirms zero bytes.
	if file.Size != 0 || strings.EqualFold(file.Md5Checksum, "d41d8cd98f00b204e9800998ecf8427e") {
		size := file.Size
		result.Size = &size
	}
	return result, nil
}
