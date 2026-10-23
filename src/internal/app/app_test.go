package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/migration"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/model"
)

type listSource struct {
	items []model.Item
	err   error
}

func (s listSource) Inventory(context.Context) ([]model.Item, error) { return s.items, s.err }
func (s listSource) File(context.Context, string) (model.SourceFile, error) {
	return model.SourceFile{}, errors.New("folders are unsupported")
}
func (s listSource) Download(context.Context, model.SourceFile, *os.File) error {
	return errors.New("unexpected download")
}

func TestInventoryOutputIsCompleteSortedJSONL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	size := int64(123)
	env := map[string]string{"MICROSOFT_CLIENT_ID": "client", "AUTH_DIR": filepath.Join(dir, ".auth")}
	deps := Dependencies{
		Source: func(_ context.Context, _ config.Config, log io.Writer) (migration.Source, error) {
			if _, err := io.WriteString(log, "Sign in instructions\n"); err != nil {
				return nil, err
			}
			return listSource{items: []model.Item{
				{ID: "b", Path: "/z", Name: "z", Type: "file", Size: &size},
				{ID: "a", Path: "/a", Name: "line\nbreak", Type: "folder"},
			}}, nil
		},
		Destination: func(context.Context, config.Config, io.Writer) (migration.Destination, error) {
			t.Fatal("list initialized Google")
			return nil, nil
		},
	}
	code := Run(context.Background(), []string{"--list-all"}, func(k string) string { return env[k] }, &stdout, &stderr, deps)
	if code != 0 || !strings.Contains(stderr.String(), "Sign in") || strings.Contains(stdout.String(), "Sign in") {
		t.Fatalf("code=%d out=%s err=%s", code, &stdout, &stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("not two JSON lines: %q", stdout.String())
	}
	for i, line := range lines {
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatal(err)
		}
		if len(item) != 5 {
			t.Errorf("unexpected JSON shape: %v", item)
		}
		for _, field := range []string{"id", "path", "name", "type", "size"} {
			if _, ok := item[field]; !ok {
				t.Errorf("missing %s", field)
			}
		}
		if i == 0 && (item["id"] != "a" || item["size"] != nil) {
			t.Errorf("not sorted or missing-size null not preserved: %v", item)
		}
	}
}

func TestInventoryErrorEmitsNoPartialOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := map[string]string{"MICROSOFT_CLIENT_ID": "client", "AUTH_DIR": filepath.Join(t.TempDir(), ".auth")}
	deps := Dependencies{Source: func(context.Context, config.Config, io.Writer) (migration.Source, error) {
		return listSource{items: []model.Item{{ID: "partial"}}, err: errors.New("page two failed")}, nil
	}}
	code := Run(context.Background(), []string{"--list-all"}, func(k string) string { return env[k] }, &stdout, &stderr, deps)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "page two failed") {
		t.Fatalf("code=%d out=%s err=%s", code, &stdout, &stderr)
	}
}

func TestHelpAndInvalidInputsDoNotInitializeProviders(t *testing.T) {
	for _, test := range []struct {
		args []string
		code int
	}{{[]string{"--help"}, 0}, {[]string{"--copy-item=/path"}, 2}, {nil, 2}} {
		var stdout, stderr bytes.Buffer
		deps := Dependencies{Source: func(context.Context, config.Config, io.Writer) (migration.Source, error) {
			t.Fatal("provider initialized for help or invalid input")
			return nil, nil
		}}
		code := Run(context.Background(), test.args, func(string) string { return "" }, &stdout, &stderr, deps)
		if code != test.code {
			t.Fatalf("args=%v code=%d expected=%d", test.args, code, test.code)
		}
	}
}

func TestFailedCopyReturnsNonzeroAndDoesNotRequireGoogle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	env := map[string]string{"MICROSOFT_CLIENT_ID": "client", "AUTH_DIR": filepath.Join(t.TempDir(), ".auth")}
	deps := Dependencies{
		Source: func(context.Context, config.Config, io.Writer) (migration.Source, error) {
			return listSource{}, nil
		},
		Destination: func(context.Context, config.Config, io.Writer) (migration.Destination, error) {
			t.Fatal("unsupported item initialized Google")
			return nil, nil
		},
	}
	code := Run(context.Background(), []string{"--copy-item=folder-id"}, func(k string) string { return env[k] }, &stdout, &stderr, deps)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "1 failed") {
		t.Fatalf("code=%d out=%s err=%s", code, &stdout, &stderr)
	}
}

type copySource struct{ listSource }

func (copySource) File(_ context.Context, id string) (model.SourceFile, error) {
	size := int64(7)
	return model.SourceFile{ID: id, Name: "file.txt", Size: &size}, nil
}
func (copySource) Download(_ context.Context, _ model.SourceFile, file *os.File) error {
	_, err := io.WriteString(file, "content")
	return err
}

type copyDestination struct{ t *testing.T }

func (copyDestination) EnsureFolder(context.Context) (string, error) {
	return "test-folder", nil
}
func (copyDestination) ExistingNames(context.Context, string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}
func (d copyDestination) Upload(_ context.Context, folder string, _ model.SourceFile, local *os.File, checksum string) (model.UploadedFile, error) {
	if folder != "test-folder" || !strings.HasSuffix(local.Name(), ".ready") {
		d.t.Fatalf("upload bypassed destination/staging: %q %q", folder, local.Name())
	}
	data, err := io.ReadAll(local)
	if err != nil || string(data) != "content" {
		d.t.Fatalf("staged content = %q, error = %v", data, err)
	}
	size := int64(len(data))
	return model.UploadedFile{ID: "uploaded", Size: &size, MD5: checksum}, nil
}

func TestEnvironmentCopyWiresStagingSkippingAndSummary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	tempDir := filepath.Join(dir, "temp-copy")
	env := map[string]string{
		"MICROSOFT_CLIENT_ID": "client",
		"COPY_ITEM_IDS":       `["first","second"]`,
		"AUTH_DIR":            filepath.Join(dir, ".auth"),
		"TEMP_COPY_DIR":       tempDir,
	}
	deps := Dependencies{
		Source: func(context.Context, config.Config, io.Writer) (migration.Source, error) {
			return copySource{}, nil
		},
		Destination: func(context.Context, config.Config, io.Writer) (migration.Destination, error) {
			return copyDestination{t}, nil
		},
	}
	code := Run(context.Background(), nil, func(k string) string { return env[k] }, &stdout, &stderr, deps)
	if code != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "1 copied, 1 skipped, 0 failed") {
		t.Fatalf("code=%d out=%s err=%s", code, &stdout, &stderr)
	}
	files, err := os.ReadDir(tempDir)
	if err != nil || len(files) != 0 {
		t.Fatalf("successful command left staging files: %v, %v", files, err)
	}
}
