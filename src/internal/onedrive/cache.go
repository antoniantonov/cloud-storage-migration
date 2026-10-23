package onedrive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/state"
)

type accountBinding struct {
	Version       int    `json:"version"`
	ClientID      string `json:"client_id"`
	Authority     string `json:"authority"`
	HomeAccountID string `json:"home_account_id"`
}

type tokenCache struct {
	mu          sync.Mutex
	dir         string
	clientID    string
	bindingPath string
	accountID   string
	staging     bool
	pending     []byte
}

func cacheKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func newTokenCache(dir, clientID string) (*tokenCache, error) {
	if err := state.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("Microsoft authentication state: %w", err)
	}
	c := &tokenCache{
		dir:         dir,
		clientID:    clientID,
		bindingPath: filepath.Join(dir, "microsoft-"+cacheKey(authority+"\x00"+clientID)+".account.json"),
	}
	data, err := state.ReadFile(c.bindingPath)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Microsoft account binding: %w", err)
	}
	var binding accountBinding
	if err := json.Unmarshal(data, &binding); err != nil {
		return nil, errors.New("Microsoft account binding is corrupt; restore it or use a new AUTH_DIR")
	}
	if binding.Version != 1 || binding.ClientID != clientID || binding.Authority != authority || binding.HomeAccountID == "" {
		return nil, errors.New("Microsoft account binding does not match this application")
	}
	c.accountID = binding.HomeAccountID
	return c, nil
}

func (c *tokenCache) path(accountID string) string {
	return filepath.Join(c.dir, "microsoft-"+cacheKey(authority+"\x00"+c.clientID+"\x00"+accountID)+".msal-cache.bin")
}

func cacheContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 10*time.Second)
}

func (c *tokenCache) Replace(ctx context.Context, target cache.Unmarshaler, _ cache.ReplaceHints) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = &safeError{retErr.Error(), retErr}
		}
	}()
	ctx, cancel := cacheContext(ctx)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.accountID == "" {
		return nil
	}
	data, err := state.ReadFile(c.path(c.accountID))
	if err != nil {
		return fmt.Errorf("read Microsoft token cache: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := target.Unmarshal(data); err != nil {
		return errors.New("Microsoft token cache is corrupt; restore it or use a new AUTH_DIR")
	}
	return ctx.Err()
}

func (c *tokenCache) Export(ctx context.Context, source cache.Marshaler, _ cache.ExportHints) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = &safeError{retErr.Error(), retErr}
		}
	}()
	ctx, cancel := cacheContext(ctx)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := source.Marshal()
	if err != nil {
		return errors.New("serialize Microsoft token cache failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// MSAL exports before returning the authenticated account. Defer writes until
	// that account is checked, so signing into a different account cannot replace it.
	if c.staging || c.accountID == "" {
		c.pending = append([]byte(nil), data...)
		return nil
	}
	if err := state.WriteFile(c.path(c.accountID), data); err != nil {
		return fmt.Errorf("persist Microsoft token cache: %w", err)
	}
	return ctx.Err()
}

func (c *tokenCache) stage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.staging = true
	c.pending = nil
}

func (c *tokenCache) discard() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.staging = false
	c.pending = nil
}

func (c *tokenCache) bind(ctx context.Context, accountID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if accountID == "" || (c.accountID != "" && c.accountID != accountID) {
		return errors.New("Microsoft sign-in used a different account; sign in with the previously authorized account or use a new AUTH_DIR")
	}
	if len(c.pending) == 0 {
		return errors.New("Microsoft authentication returned no token cache to persist")
	}
	if err := state.WriteFile(c.path(accountID), c.pending); err != nil {
		return fmt.Errorf("persist Microsoft token cache: %w", err)
	}
	if c.accountID == "" {
		data, err := json.Marshal(accountBinding{1, c.clientID, authority, accountID})
		if err != nil {
			return errors.New("serialize Microsoft account binding failed")
		}
		if err := state.WriteFile(c.bindingPath, data); err != nil {
			return fmt.Errorf("persist Microsoft account binding: %w", err)
		}
	}
	c.accountID = accountID
	c.staging = false
	c.pending = nil
	return ctx.Err()
}
