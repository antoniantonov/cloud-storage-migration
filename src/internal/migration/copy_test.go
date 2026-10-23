package migration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
)

type fakeSource struct {
	files     map[string]model.SourceFile
	download  func(context.Context, model.SourceFile, *os.File) error
	downloads int
}

func (f *fakeSource) Inventory(context.Context) ([]model.Item, error) { return nil, nil }
func (f *fakeSource) File(_ context.Context, id string) (model.SourceFile, error) {
	file, ok := f.files[id]
	if !ok {
		return model.SourceFile{}, errors.New("unsupported folder or missing ID")
	}
	return file, nil
}
func (f *fakeSource) Download(ctx context.Context, file model.SourceFile, local *os.File) error {
	f.downloads++
	if f.download != nil {
		return f.download(ctx, file, local)
	}
	_, err := io.WriteString(local, "content")
	return err
}

type fakeDestination struct {
	names       map[string]struct{}
	nameQueries int
	uploads     int
	folderErr   error
	upload      func(model.SourceFile, *os.File, string) (model.UploadedFile, error)
}

func (f *fakeDestination) EnsureFolder(context.Context) (string, error) {
	return "destination", f.folderErr
}
func (f *fakeDestination) ExistingNames(context.Context, string) (map[string]struct{}, error) {
	f.nameQueries++
	result := make(map[string]struct{})
	for name := range f.names {
		result[name] = struct{}{}
	}
	return result, nil
}
func (f *fakeDestination) Upload(_ context.Context, _ string, file model.SourceFile, local *os.File, checksum string) (model.UploadedFile, error) {
	f.uploads++
	if f.upload != nil {
		return f.upload(file, local, checksum)
	}
	hash := md5.New()
	size, err := io.Copy(hash, local)
	if err != nil {
		return model.UploadedFile{}, err
	}
	if checksum != hex.EncodeToString(hash.Sum(nil)) {
		return model.UploadedFile{}, errors.New("wrong local checksum")
	}
	if f.names == nil {
		f.names = make(map[string]struct{})
	}
	f.names[file.Name] = struct{}{}
	return model.UploadedFile{ID: "uploaded", Size: &size, MD5: checksum}, nil
}

func sourceFile(id, name string) model.SourceFile {
	size := int64(len("content"))
	return model.SourceFile{ID: id, Name: name, Size: &size}
}

func runCopy(t *testing.T, ctx context.Context, ids []string, source *fakeSource, destination *fakeDestination, dir string) (Summary, []Result, error) {
	t.Helper()
	var results []Result
	summary, err := Run(ctx, ids, source, func(context.Context) (Destination, error) {
		return destination, nil
	}, dir, func(result Result) error {
		results = append(results, result)
		return nil
	})
	return summary, results, err
}

func TestMixedBatchSkipsAndContinues(t *testing.T) {
	source := &fakeSource{files: map[string]model.SourceFile{
		"one":   sourceFile("one", "exists"),
		"two":   sourceFile("two", "new"),
		"three": sourceFile("three", "new"),
	}}
	destination := &fakeDestination{names: map[string]struct{}{"exists": {}}}
	dir := filepath.Join(t.TempDir(), "temp-copy")
	summary, results, err := runCopy(t, context.Background(), []string{"folder", "one", "two", "three"}, source, destination, dir)
	if err != nil || summary != (Summary{Copied: 1, Skipped: 2, Failed: 1}) {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if len(results) != 4 || source.downloads != 1 || destination.uploads != 1 {
		t.Fatalf("results=%d downloads=%d uploads=%d", len(results), source.downloads, destination.uploads)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("successful upload left files: %v, %v", files, err)
	}
}

func TestUnsupportedIDsNeverInitializeGoogle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "temp-copy")
	summary, err := Run(context.Background(), []string{"folder"}, &fakeSource{}, func(context.Context) (Destination, error) {
		t.Fatal("Google initialized for unsupported source")
		return nil, nil
	}, dir, func(Result) error { return nil })
	if err != nil || summary.Failed != 1 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging created before needed: %v", err)
	}
}

func TestUploadFailureRetainsCompletedDownload(t *testing.T) {
	source := &fakeSource{files: map[string]model.SourceFile{"one": sourceFile("one", "../remote-name")}}
	destination := &fakeDestination{upload: func(model.SourceFile, *os.File, string) (model.UploadedFile, error) {
		return model.UploadedFile{}, errors.New("upload unavailable")
	}}
	dir := filepath.Join(t.TempDir(), "temp-copy")
	summary, results, err := runCopy(t, context.Background(), []string{"one"}, source, destination, dir)
	if err != nil || summary.Failed != 1 || len(results) != 1 {
		t.Fatalf("summary=%+v results=%v error=%v", summary, results, err)
	}
	path := results[0].TempPath
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, ".ready") {
		t.Fatalf("unsafe or incomplete retained path: %q", path)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "content" {
		t.Fatalf("retained data=%q error=%v", content, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("retained file permissions: %v, %v", info, err)
	}
}

func TestPartialAndTruncatedDownloadsAreRemoved(t *testing.T) {
	for _, returnedError := range []error{errors.New("connection lost"), nil} {
		t.Run(stringOr(returnedError, "truncated"), func(t *testing.T) {
			source := &fakeSource{files: map[string]model.SourceFile{"one": sourceFile("one", "file")}, download: func(_ context.Context, _ model.SourceFile, local *os.File) error {
				if _, err := io.WriteString(local, "part"); err != nil {
					return err
				}
				return returnedError
			}}
			destination := &fakeDestination{}
			dir := filepath.Join(t.TempDir(), "temp-copy")
			summary, _, err := runCopy(t, context.Background(), []string{"one"}, source, destination, dir)
			files, readErr := os.ReadDir(dir)
			if err != nil || readErr != nil || summary.Failed != 1 || len(files) != 0 || destination.uploads != 0 {
				t.Fatalf("summary=%+v files=%v error=%v read=%v uploads=%d", summary, files, err, readErr, destination.uploads)
			}
		})
	}
}

func stringOr(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

func TestAmbiguousUploadRefreshesNamesBeforeNextSelection(t *testing.T) {
	source := &fakeSource{files: map[string]model.SourceFile{
		"one": sourceFile("one", "same"),
		"two": sourceFile("two", "same"),
	}}
	destination := &fakeDestination{names: make(map[string]struct{})}
	destination.upload = func(file model.SourceFile, _ *os.File, _ string) (model.UploadedFile, error) {
		destination.names[file.Name] = struct{}{}
		return model.UploadedFile{}, errors.New("response lost after creation")
	}
	summary, _, err := runCopy(t, context.Background(), []string{"one", "two"}, source, destination, filepath.Join(t.TempDir(), "temp-copy"))
	if err != nil || summary != (Summary{Failed: 1, Skipped: 1}) || destination.uploads != 1 || destination.nameQueries != 2 {
		t.Fatalf("summary=%+v error=%v uploads=%d queries=%d", summary, err, destination.uploads, destination.nameQueries)
	}
}

func TestUploadedMetadataMismatchRetainsLocalFile(t *testing.T) {
	for _, metadata := range []model.UploadedFile{{}, {ID: "new", MD5: "wrong"}, {ID: "new", Size: new(int64)}} {
		source := &fakeSource{files: map[string]model.SourceFile{"one": sourceFile("one", "file")}}
		destination := &fakeDestination{upload: func(model.SourceFile, *os.File, string) (model.UploadedFile, error) {
			return metadata, nil
		}}
		summary, results, err := runCopy(t, context.Background(), []string{"one"}, source, destination, filepath.Join(t.TempDir(), "temp-copy"))
		if err != nil || summary.Failed != 1 || summary.Copied != 0 || results[0].TempPath == "" {
			t.Fatalf("unverified upload accepted: %+v, %v, %v", summary, results, err)
		}
	}
}

func TestUploadedCleanupFailureIsNotRetried(t *testing.T) {
	source := &fakeSource{files: map[string]model.SourceFile{"one": sourceFile("one", "file")}}
	destination := &fakeDestination{upload: func(_ model.SourceFile, local *os.File, checksum string) (model.UploadedFile, error) {
		if err := os.Remove(local.Name()); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(local.Name(), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(local.Name(), "block"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		return model.UploadedFile{ID: "new", MD5: checksum}, nil
	}}
	summary, results, err := runCopy(t, context.Background(), []string{"one"}, source, destination, filepath.Join(t.TempDir(), "temp-copy"))
	if err != nil || summary != (Summary{Copied: 1, Failed: 1}) || destination.uploads != 1 || results[0].Status != "uploaded, local cleanup failed" {
		t.Fatalf("summary=%+v results=%v error=%v uploads=%d", summary, results, err, destination.uploads)
	}
}

func TestCanceledDownloadRemovesPartialAndStopsBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &fakeSource{files: map[string]model.SourceFile{"one": sourceFile("one", "file")}}
	source.download = func(_ context.Context, _ model.SourceFile, local *os.File) error {
		_, err := io.WriteString(local, "part")
		cancel()
		return err
	}
	destination := &fakeDestination{}
	dir := filepath.Join(t.TempDir(), "temp-copy")
	summary, _, err := runCopy(t, ctx, []string{"one", "two"}, source, destination, dir)
	files, readErr := os.ReadDir(dir)
	if !errors.Is(err, context.Canceled) || readErr != nil || summary.Failed != 1 || len(files) != 0 || destination.uploads != 0 {
		t.Fatalf("summary=%+v files=%v error=%v read=%v", summary, files, err, readErr)
	}
}

func TestLargeFileUsesBoundedMemory(t *testing.T) {
	size := int64(64 * 1024 * 1024)
	source := &fakeSource{files: map[string]model.SourceFile{
		"large": {ID: "large", Name: "large.bin", Size: &size},
	}}
	source.download = func(ctx context.Context, _ model.SourceFile, local *os.File) error {
		chunk := bytes.Repeat([]byte{42}, 64*1024)
		for written := int64(0); written < size; written += int64(len(chunk)) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := local.Write(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	destination := &fakeDestination{}
	dir := filepath.Join(t.TempDir(), "temp-copy")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	summary, _, err := runCopy(t, context.Background(), []string{"large"}, source, destination, dir)
	runtime.ReadMemStats(&after)
	if err != nil || summary.Copied != 1 || summary.Failed != 0 {
		t.Fatalf("large copy: %+v, %v", summary, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8*1024*1024 {
		t.Fatalf("allocated %d bytes staging a 64 MiB file; want <=8 MiB", allocated)
	}
}
