package migration

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/state"
)

type Summary struct {
	Copied  int
	Skipped int
	Failed  int
}

type Result struct {
	ID       string
	Name     string
	Status   string
	TempPath string
	Err      error
}

type DestinationFactory func(context.Context) (Destination, error)

func Run(ctx context.Context, ids []string, source Source, newDestination DestinationFactory, tempDir string, report func(Result) error) (Summary, error) {
	var summary Summary
	var destination Destination
	var folderID string
	var names map[string]struct{}
	namesDirty := false
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		file, err := source.File(ctx, id)
		if err == nil && (file.ID == "" || file.Name == "") {
			err = errors.New("source metadata is missing an item ID or filename")
		}
		if err != nil {
			summary.Failed++
			if reportErr := report(Result{ID: id, Status: "failed", Err: err}); reportErr != nil {
				return summary, reportErr
			}
			continue
		}
		if destination == nil {
			destination, err = newDestination(ctx)
			if err != nil {
				return summary, fmt.Errorf("initialize Google Drive: %w", err)
			}
			folderID, err = destination.EnsureFolder(ctx)
			if err != nil {
				return summary, fmt.Errorf("resolve test-migration: %w", err)
			}
			if folderID == "" {
				return summary, errors.New("Google Drive returned an empty destination folder ID")
			}
			namesDirty = true
		}
		if namesDirty {
			names, err = destination.ExistingNames(ctx, folderID)
			if err != nil {
				return summary, fmt.Errorf("discover destination names: %w", err)
			}
			if names == nil {
				names = make(map[string]struct{})
			}
			namesDirty = false
		}
		if _, exists := names[file.Name]; exists {
			summary.Skipped++
			if err := report(Result{ID: id, Name: file.Name, Status: "skipped (same name already exists)"}); err != nil {
				return summary, err
			}
			continue
		}
		result, uploaded, attempted := copyFile(ctx, source, destination, folderID, file, tempDir)
		if uploaded {
			names[file.Name] = struct{}{}
			summary.Copied++
		} else if attempted {
			// A failed upload response can still mean the object was created.
			namesDirty = true
		}
		if result.Err != nil {
			summary.Failed++
		}
		if err := report(result); err != nil {
			return summary, err
		}
	}
	return summary, ctx.Err()
}

func copyFile(ctx context.Context, source Source, destination Destination, folderID string, file model.SourceFile, tempDir string) (Result, bool, bool) {
	result := Result{ID: file.ID, Name: file.Name, Status: "failed"}
	readyPath, checksum, err := stage(ctx, source, file, tempDir)
	if err != nil {
		result.Err = err
		return result, false, false
	}
	result.TempPath = readyPath
	if err := ctx.Err(); err != nil {
		result.Err = fmt.Errorf("copy canceled after download: %w", err)
		return result, false, false
	}
	local, err := os.Open(readyPath)
	if err != nil {
		result.Err = fmt.Errorf("open completed download: %w", err)
		return result, false, false
	}
	info, err := local.Stat()
	if err != nil {
		result.Err = errors.Join(fmt.Errorf("inspect completed download: %w", err), local.Close())
		return result, false, false
	}
	uploaded, uploadErr := destination.Upload(ctx, folderID, file, local, checksum)
	closeErr := local.Close()
	if uploadErr == nil {
		switch {
		case uploaded.ID == "":
			uploadErr = errors.New("upload response is missing its file ID")
		case uploaded.Size != nil && *uploaded.Size != info.Size():
			uploadErr = errors.New("uploaded size does not match the completed download")
		case uploaded.MD5 != "" && !strings.EqualFold(uploaded.MD5, checksum):
			uploadErr = errors.New("uploaded checksum does not match the completed download")
		}
	}
	if uploadErr != nil {
		result.Err = errors.Join(fmt.Errorf("upload failed: %w", uploadErr), closeErr)
		return result, false, true
	}
	if closeErr != nil {
		result.Status = "uploaded, local cleanup failed"
		result.Err = fmt.Errorf("close uploaded local file: %w", closeErr)
		return result, true, true
	}
	if err := os.Remove(readyPath); err != nil {
		result.Status = "uploaded, local cleanup failed"
		result.Err = fmt.Errorf("remove uploaded local file: %w", err)
		return result, true, true
	}
	result.Status = "copied"
	result.TempPath = ""
	return result, true, true
}

func stage(ctx context.Context, source Source, file model.SourceFile, tempDir string) (string, string, error) {
	if err := state.EnsureDir(tempDir); err != nil {
		return "", "", fmt.Errorf("prepare temp-copy directory: %w", err)
	}
	local, err := os.CreateTemp(tempDir, "copy-*.part")
	if err != nil {
		return "", "", fmt.Errorf("create temporary download: %w", err)
	}
	path := local.Name()
	downloadErr := source.Download(ctx, file, local)
	if downloadErr == nil {
		downloadErr = ctx.Err()
	}
	if downloadErr == nil {
		info, err := local.Stat()
		if err != nil {
			downloadErr = err
		} else if file.Size != nil && info.Size() != *file.Size {
			downloadErr = errors.New("downloaded size does not match source metadata")
		}
	}
	var checksum string
	if downloadErr == nil {
		if _, err := local.Seek(0, io.SeekStart); err != nil {
			downloadErr = err
		} else {
			hash := md5.New()
			if _, err := io.Copy(hash, contextReader{ctx, local}); err != nil {
				downloadErr = err
			} else {
				checksum = hex.EncodeToString(hash.Sum(nil))
			}
		}
	}
	if downloadErr == nil {
		downloadErr = local.Sync()
	}
	downloadErr = errors.Join(downloadErr, local.Close())
	if downloadErr != nil {
		return "", "", removePartial(path, fmt.Errorf("download failed: %w", downloadErr))
	}
	readyPath := strings.TrimSuffix(path, ".part") + ".ready"
	if err := os.Rename(path, readyPath); err != nil {
		return "", "", removePartial(path, fmt.Errorf("finalize download: %w", err))
	}
	return readyPath, checksum, nil
}

func removePartial(path string, cause error) error {
	if err := os.Remove(path); err != nil {
		return errors.Join(cause, fmt.Errorf("remove partial download %q: %w", path, err))
	}
	return cause
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
