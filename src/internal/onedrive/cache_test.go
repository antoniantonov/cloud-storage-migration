package onedrive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
)

type cacheBytes struct {
	data []byte
	err  error
}

func (b *cacheBytes) Marshal() ([]byte, error) { return b.data, b.err }
func (b *cacheBytes) Unmarshal(data []byte) error {
	b.data = append([]byte(nil), data...)
	return b.err
}

func TestOpaqueCachePersistenceAndAccountIsolation(t *testing.T) {
	dir := workDir(t)
	c, err := newTokenCache(dir, "app-one")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := &cacheBytes{data: []byte("opaque\x00token-cache")}
	if err := c.Export(ctx, payload, cache.ExportHints{PartitionKey: "untrusted-hint"}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("unbound authentication cache was persisted")
	}
	if err := c.bind(ctx, "home-account"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{c.bindingPath, c.path("home-account")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private cache permissions: %v, %v", info, err)
		}
	}
	reopened, err := newTokenCache(dir, "app-one")
	if err != nil {
		t.Fatal(err)
	}
	var got cacheBytes
	if err := reopened.Replace(ctx, &got, cache.ReplaceHints{PartitionKey: "different-hint"}); err != nil || !bytes.Equal(got.data, payload.data) {
		t.Fatalf("cache roundtrip: %q, %v", got.data, err)
	}
	other, err := newTokenCache(dir, "app-two")
	if err != nil || other.accountID != "" || other.bindingPath == c.bindingPath || other.path("home-account") == c.path("home-account") {
		t.Fatalf("cache applications not isolated: %#v, %v", other, err)
	}
	if c.path("other-account") == c.path("home-account") {
		t.Fatal("cache path does not incorporate the account")
	}
	relative, err := filepath.Rel(dir, c.path("../../outside"))
	if err != nil || strings.HasPrefix(relative, "..") || filepath.IsAbs(relative) {
		t.Fatal("account ID can escape cache directory")
	}
}

func TestStagedDifferentAccountDoesNotOverwriteCache(t *testing.T) {
	c, err := newTokenCache(workDir(t), "app")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Export(ctx, &cacheBytes{data: []byte("old-cache")}, cache.ExportHints{}); err != nil {
		t.Fatal(err)
	}
	if err := c.bind(ctx, "intended"); err != nil {
		t.Fatal(err)
	}
	c.stage()
	if err := c.Export(ctx, &cacheBytes{data: []byte("other-account-cache")}, cache.ExportHints{}); err != nil {
		t.Fatal(err)
	}
	if err := c.bind(ctx, "other"); err == nil {
		t.Fatal("accepted a changed account")
	}
	c.discard()
	data, err := os.ReadFile(c.path("intended"))
	if err != nil || string(data) != "old-cache" {
		t.Fatalf("wrong account overwrote cache: %q, %v", data, err)
	}
}

func TestCacheErrorsAreNotTreatedAsMissingCache(t *testing.T) {
	t.Run("binding corrupt", func(t *testing.T) {
		dir := workDir(t)
		c, err := newTokenCache(dir, "app")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.bindingPath, []byte("SECRET_CORRUPT_CONTENT"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newTokenCache(dir, "app"); err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("unsafe corruption error %v", err)
		}
	})
	t.Run("binding mismatched", func(t *testing.T) {
		dir := workDir(t)
		c, err := newTokenCache(dir, "app")
		if err != nil {
			t.Fatal(err)
		}
		body := marshalJSON(t, accountBinding{1, "different", authority, "account"})
		if err := os.WriteFile(c.bindingPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := newTokenCache(dir, "app"); err == nil {
			t.Fatal("accepted another application binding")
		}
	})
	for _, mode := range []string{"missing", "public", "corrupt", "directory"} {
		t.Run(mode, func(t *testing.T) {
			c, err := newTokenCache(workDir(t), "app")
			if err != nil {
				t.Fatal(err)
			}
			c.accountID = "account"
			reader := &cacheBytes{}
			switch mode {
			case "public", "corrupt":
				if err := os.WriteFile(c.path(c.accountID), []byte("opaque"), 0600); err != nil {
					t.Fatal(err)
				}
				if mode == "public" {
					if err := os.Chmod(c.path(c.accountID), 0644); err != nil {
						t.Fatal(err)
					}
				} else {
					reader.err = errors.New("SECRET_BAD_CACHE")
				}
			case "directory":
				if err := os.Mkdir(c.path(c.accountID), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Replace(context.Background(), reader, cache.ReplaceHints{}); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("missing or unsafe cache error %v", err)
			}
		})
	}
}

func TestCacheWriteFailuresAndCancellation(t *testing.T) {
	c, err := newTokenCache(workDir(t), "app")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.accountID = "account"
	if err := os.Mkdir(c.path(c.accountID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.Export(ctx, &cacheBytes{data: []byte("data")}, cache.ExportHints{}); err == nil {
		t.Fatal("suppressed persistence error")
	}
	if err := c.Export(ctx, &cacheBytes{err: errors.New("SECRET")}, cache.ExportHints{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("marshal error: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for _, err := range []error{
		c.Export(canceled, &cacheBytes{}, cache.ExportHints{}),
		c.Replace(canceled, &cacheBytes{}, cache.ReplaceHints{}),
		c.bind(canceled, "account"),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not honored: %v", err)
		}
	}
}

func TestAccountBindingWriteFailureIsReported(t *testing.T) {
	c, err := newTokenCache(workDir(t), "app")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.bindingPath, 0700); err != nil {
		t.Fatal(err)
	}
	c.pending = []byte("opaque")
	if err := c.bind(context.Background(), "account"); err == nil || c.accountID != "" {
		t.Fatalf("binding persistence failure suppressed: %v", err)
	}
}
