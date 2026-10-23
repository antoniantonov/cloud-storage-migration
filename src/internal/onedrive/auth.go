package onedrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	msalerrors "github.com/AzureAD/microsoft-authentication-library-for-go/apps/errors"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
)

const (
	authority = "https://login.microsoftonline.com/consumers"
	fileScope = "https://graph.microsoft.com/Files.Read"
)

type deviceLogin struct {
	message string
	wait    func(context.Context) (public.AuthResult, error)
}

type authenticationClient interface {
	accounts(context.Context) ([]public.Account, error)
	silent(context.Context, public.Account, string) (public.AuthResult, error)
	device(context.Context, string) (deviceLogin, error)
}

type msalClient struct{ client public.Client }

func (m msalClient) accounts(ctx context.Context) ([]public.Account, error) {
	return m.client.Accounts(ctx)
}

func (m msalClient) silent(ctx context.Context, account public.Account, claims string) (public.AuthResult, error) {
	return m.client.AcquireTokenSilent(ctx, []string{fileScope}, public.WithSilentAccount(account), public.WithClaims(claims))
}

func (m msalClient) device(ctx context.Context, claims string) (deviceLogin, error) {
	code, err := m.client.AcquireTokenByDeviceCode(ctx, []string{fileScope}, public.WithClaims(claims))
	if err != nil {
		return deviceLogin{}, err
	}
	return deviceLogin{message: code.Result.Message, wait: code.AuthenticationResult}, nil
}

type credential struct {
	mu          sync.Mutex
	client      authenticationClient
	cache       *tokenCache
	interactive bool
	stderr      io.Writer
}

var _ azcore.TokenCredential = (*credential)(nil)

func newCredential(cfg config.Config, stderr io.Writer) (*credential, error) {
	if cfg.MicrosoftClientID == "" {
		return nil, errors.New("MICROSOFT_CLIENT_ID must identify a registered Microsoft public-client application")
	}
	c, err := newTokenCache(cfg.AuthDir, cfg.MicrosoftClientID)
	if err != nil {
		return nil, err
	}
	client, err := public.New(cfg.MicrosoftClientID, public.WithAuthority(authority), public.WithCache(c))
	if err != nil {
		return nil, safeFailure("initialize Microsoft public-client authentication", err)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &credential{client: msalClient{client}, cache: c, interactive: cfg.InteractiveAuth, stderr: stderr}, nil
}

func (c *credential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (token azcore.AccessToken, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = &safeError{retErr.Error(), retErr}
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return azcore.AccessToken{}, err
	}
	if options.TenantID != "" && options.TenantID != "consumers" {
		return azcore.AccessToken{}, errors.New("Microsoft authentication supports the consumers authority only")
	}
	for _, scope := range options.Scopes {
		if scope != fileScope {
			return azcore.AccessToken{}, errors.New("Microsoft authentication supports delegated Files.Read only")
		}
	}
	accounts, err := c.client.accounts(ctx)
	if err != nil {
		return azcore.AccessToken{}, safeFailure("load Microsoft cached accounts (check private cache permissions and integrity)", err)
	}
	account, found, err := selectAccount(accounts, c.cache.accountID)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	if found {
		result, err := c.client.silent(ctx, account, options.Claims)
		if err == nil {
			return c.accessToken(result)
		}
		if !requiresInteraction(err) {
			return azcore.AccessToken{}, safeFailure("silently authenticate Microsoft account (check connectivity and private cache)", err)
		}
	}
	if !c.interactive {
		return azcore.AccessToken{}, errors.New("Microsoft sign-in is required but AUTH_INTERACTIVE=false; run interactively to authorize this account")
	}
	authCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	c.cache.stage()
	defer c.cache.discard()
	login, err := c.client.device(authCtx, options.Claims)
	if err != nil {
		return azcore.AccessToken{}, safeFailure("start Microsoft device-code sign-in", err)
	}
	if login.message == "" || login.wait == nil {
		return azcore.AccessToken{}, errors.New("Microsoft device-code response has no login instructions")
	}
	if _, err := fmt.Fprintln(c.stderr, login.message); err != nil {
		return azcore.AccessToken{}, fmt.Errorf("write Microsoft login instructions: %w", err)
	}
	result, err := login.wait(authCtx)
	if err != nil {
		return azcore.AccessToken{}, safeFailure("complete Microsoft device-code sign-in", err)
	}
	token, err = c.accessToken(result)
	if err != nil {
		return azcore.AccessToken{}, err
	}
	if err := c.cache.bind(ctx, result.Account.HomeAccountID); err != nil {
		return azcore.AccessToken{}, err
	}
	return token, nil
}

func selectAccount(accounts []public.Account, intended string) (public.Account, bool, error) {
	if intended == "" {
		if len(accounts) != 0 {
			return public.Account{}, false, errors.New("Microsoft cache contains accounts without an intended account binding; use a new AUTH_DIR")
		}
		return public.Account{}, false, nil
	}
	var selected public.Account
	found := false
	for _, account := range accounts {
		if account.HomeAccountID != intended {
			return public.Account{}, false, errors.New("Microsoft cache contains an unexpected account; refusing ambiguous account selection")
		}
		if found {
			return public.Account{}, false, errors.New("Microsoft cache contains ambiguous records for the intended account")
		}
		selected, found = account, true
	}
	return selected, found, nil
}

func (c *credential) accessToken(result public.AuthResult) (azcore.AccessToken, error) {
	if result.Account.HomeAccountID == "" || (c.cache.accountID != "" && result.Account.HomeAccountID != c.cache.accountID) {
		return azcore.AccessToken{}, errors.New("Microsoft authentication returned a different or missing account identity")
	}
	if result.AccessToken == "" || !result.ExpiresOn.After(time.Now()) {
		return azcore.AccessToken{}, errors.New("Microsoft authentication returned an empty or expired access token")
	}
	return azcore.AccessToken{Token: result.AccessToken, ExpiresOn: result.ExpiresOn}, nil
}

func requiresInteraction(err error) bool {
	// MSAL v1.9.0 has no exported cache-miss/interaction-required error type.
	if err != nil && err.Error() == "no token found" {
		return true
	}
	var call msalerrors.CallErr
	if !errors.As(err, &call) || call.Resp == nil || call.Resp.StatusCode != http.StatusBadRequest || call.Resp.Body == nil {
		return false
	}
	data, readErr := io.ReadAll(io.LimitReader(call.Resp.Body, 64*1024+1))
	call.Resp.Body = io.NopCloser(bytes.NewReader(data))
	if readErr != nil || len(data) > 64*1024 {
		return false
	}
	var response struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &response) != nil {
		return false
	}
	switch response.Error {
	case "interaction_required", "login_required", "consent_required", "invalid_grant":
		return true
	default:
		return false
	}
}

type safeError struct {
	message string
	cause   error
}

func (e *safeError) Error() string { return e.message }
func (e *safeError) Unwrap() error { return e.cause }

func safeFailure(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return &safeError{operation + ": canceled", context.Canceled}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &safeError{operation + ": deadline exceeded", context.DeadlineExceeded}
	}
	var safe *safeError
	if errors.As(err, &safe) {
		return &safeError{operation + ": " + safe.message, err}
	}
	var status interface{ GetStatusCode() int }
	if errors.As(err, &status) {
		return &safeError{fmt.Sprintf("%s: Microsoft Graph HTTP %d", operation, status.GetStatusCode()), err}
	}
	var call msalerrors.CallErr
	if errors.As(err, &call) && call.Resp != nil {
		return &safeError{fmt.Sprintf("%s: Microsoft authentication HTTP %d", operation, call.Resp.StatusCode), err}
	}
	return &safeError{operation + ": request failed", err}
}
