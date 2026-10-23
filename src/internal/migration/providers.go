package migration

import (
	"context"
	"os"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
)

type Source interface {
	Inventory(context.Context) ([]model.Item, error)
	File(context.Context, string) (model.SourceFile, error)
	// Download may rewind/truncate the file for bounded retries, but must not close it.
	Download(context.Context, model.SourceFile, *os.File) error
}

type Destination interface {
	EnsureFolder(context.Context) (string, error)
	ExistingNames(context.Context, string) (map[string]struct{}, error)
	// Upload receives the local MD5 and an open file; it must not close the file.
	Upload(context.Context, string, model.SourceFile, *os.File, string) (model.UploadedFile, error)
}
