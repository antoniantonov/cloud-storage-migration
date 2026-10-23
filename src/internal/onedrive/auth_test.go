package onedrive

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	msalerrors "github.com/AzureAD/microsoft-authentication-library-for-go/apps/errors"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
)

type fakeAuthentication struct {
	accountList []public.Account
	accountsErr error
	silentCall  func(context.Context, public.Account, string) (public.AuthResult, error)
	deviceCall  func(context.Context, string) (deviceLogin, error)
}

func (f *fakeAuthentication) accounts(context.Context) ([]public.Account, error) {
	return f.accountList, f.accountsErr
}
func (f *fakeAuthentication) silent(ctx context.Context, account public.Account, claims string) (public.AuthResult, error) {
	return f.silentCall(ctx, account, claims)
}
func (f *fakeAuthentication) device(ctx context.Context, claims string) (deviceLogin, error) {
	return f.deviceCall(ctx, claims)
}

func authResult(account string) public.AuthResult {
	return public.AuthResult{Account: public.Account{HomeAccountID: account}, AccessToken: "offline-access-token", ExpiresOn: time.Now().Add(time.Hour)}
}

func fakeCredential(t *testing.T) (*credential, *fakeAuthentication, *bytes.Buffer) {
	t.Helper()
	persistence, err := newTokenCache(workDir(t), "test-app")
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	fake := &fakeAuthentication{
		silentCall: func(context.Context, public.Account, string) (public.AuthResult, error) {
			t.Fatal("unexpected silent authentication")
			return public.AuthResult{}, nil
		},
		deviceCall: func(context.Context, string) (deviceLogin, error) {
			t.Fatal("unexpected interactive authentication")
			return deviceLogin{}, nil
		},
	}
	return &credential{cache: persistence, client: fake, interactive: true, stderr: output}, fake, output
}

func TestCredentialDisabledInteraction(t *testing.T) {
	c, _, output := fakeCredential(t)
	c.interactive = false
	if _, err := c.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{fileScope}}); err == nil || !strings.Contains(err.Error(), "AUTH_INTERACTIVE=false") {
		t.Fatalf("disabled interaction error: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("printed login instructions in noninteractive mode")
	}
}

func TestCredentialUsesBoundAccountSilently(t *testing.T) {
	c, fake, output := fakeCredential(t)
	c.cache.accountID = "intended"
	c.interactive = false
	fake.accountList = []public.Account{{HomeAccountID: "intended"}}
	fake.silentCall = func(ctx context.Context, account public.Account, claims string) (public.AuthResult, error) {
		if account.HomeAccountID != "intended" || claims != "required-claims" {
			t.Fatalf("wrong account or claims: %#v, %q", account, claims)
		}
		return authResult("intended"), nil
	}
	token, err := c.GetToken(context.Background(), policy.TokenRequestOptions{Scopes: []string{fileScope}, Claims: "required-claims"})
	if err != nil || token.Token != "offline-access-token" || output.Len() != 0 {
		t.Fatalf("silent token error=%v, output=%q", err, output.String())
	}
}

func TestCredentialDeviceFlowPersistsBeforeReturning(t *testing.T) {
	c, fake, output := fakeCredential(t)
	fake.deviceCall = func(ctx context.Context, _ string) (deviceLogin, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("interactive authentication has no deadline")
		}
		return deviceLogin{
			message: "Visit https://microsoft.com/devicelogin and enter OFFLINE.",
			wait: func(ctx context.Context) (public.AuthResult, error) {
				if err := c.cache.Export(ctx, &cacheBytes{data: []byte("opaque-cache")}, cache.ExportHints{}); err != nil {
					t.Fatal(err)
				}
				return authResult("intended"), nil
			},
		}, nil
	}
	if _, err := c.GetToken(context.Background(), policy.TokenRequestOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "OFFLINE") || strings.Contains(output.String(), "offline-access-token") {
		t.Fatalf("wrong login output %q", output.String())
	}
	reopened, err := newTokenCache(c.cache.dir, "test-app")
	if err != nil || reopened.accountID != "intended" {
		t.Fatalf("account was not persisted: %#v, %v", reopened, err)
	}
	if data, err := os.ReadFile(reopened.path("intended")); err != nil || string(data) != "opaque-cache" {
		t.Fatalf("cache not persisted: %q, %v", data, err)
	}
}

type rejectingWriter struct{}

func (rejectingWriter) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestCredentialDeviceFlowFailures(t *testing.T) {
	for _, mode := range []string{"denied", "canceled", "instructions", "different account", "persistence"} {
		t.Run(mode, func(t *testing.T) {
			c, fake, _ := fakeCredential(t)
			if mode == "different account" {
				c.cache.accountID = "previous-account"
			}
			if mode == "instructions" {
				c.stderr = rejectingWriter{}
			}
			if mode == "persistence" {
				if err := os.Mkdir(c.cache.bindingPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fake.deviceCall = func(context.Context, string) (deviceLogin, error) {
				return deviceLogin{message: "offline instructions", wait: func(ctx context.Context) (public.AuthResult, error) {
					switch mode {
					case "denied":
						return public.AuthResult{}, errors.New("SECRET access_denied")
					case "canceled":
						return public.AuthResult{}, context.Canceled
					}
					if err := c.cache.Export(ctx, &cacheBytes{data: []byte("opaque")}, cache.ExportHints{}); err != nil {
						return public.AuthResult{}, err
					}
					return authResult("new-account"), nil
				}}, nil
			}
			_, err := c.GetToken(context.Background(), policy.TokenRequestOptions{})
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("device failure not reported safely: %v", err)
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if c.cache.pending != nil || c.cache.staging {
				t.Fatal("unsuccessful device flow retained staged authentication data")
			}
		})
	}
}

func TestCredentialDoesNotPromptForArbitraryErrors(t *testing.T) {
	for _, phase := range []string{"accounts", "silent"} {
		t.Run(phase, func(t *testing.T) {
			c, fake, _ := fakeCredential(t)
			failure := errors.New("network-or-cache-failure SECRET")
			if phase == "accounts" {
				fake.accountsErr = failure
			} else {
				c.cache.accountID = "intended"
				fake.accountList = []public.Account{{HomeAccountID: "intended"}}
				fake.silentCall = func(context.Context, public.Account, string) (public.AuthResult, error) {
					return public.AuthResult{}, failure
				}
			}
			_, err := c.GetToken(context.Background(), policy.TokenRequestOptions{})
			if !errors.Is(err, failure) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error not propagated safely: %v", err)
			}
		})
	}
}

func TestCredentialRejectsAmbiguousOrMismatchedAccounts(t *testing.T) {
	for _, test := range []struct {
		name     string
		intended string
		accounts []public.Account
	}{
		{"unbound", "", []public.Account{{HomeAccountID: "other"}}},
		{"wrong", "intended", []public.Account{{HomeAccountID: "other"}}},
		{"multiple", "intended", []public.Account{{HomeAccountID: "intended"}, {HomeAccountID: "other"}}},
		{"duplicate", "intended", []public.Account{{HomeAccountID: "intended"}, {HomeAccountID: "intended"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, fake, _ := fakeCredential(t)
			c.cache.accountID, fake.accountList = test.intended, test.accounts
			if _, err := c.GetToken(context.Background(), policy.TokenRequestOptions{}); err == nil {
				t.Fatal("accepted ambiguous account state")
			}
		})
	}
}

func TestCredentialSilentCacheMissAllowsDeviceFlow(t *testing.T) {
	c, fake, _ := fakeCredential(t)
	c.cache.accountID = "intended"
	fake.accountList = []public.Account{{HomeAccountID: "intended"}}
	fake.silentCall = func(context.Context, public.Account, string) (public.AuthResult, error) {
		return public.AuthResult{}, errors.New("no token found")
	}
	called := false
	fake.deviceCall = func(context.Context, string) (deviceLogin, error) {
		called = true
		return deviceLogin{}, errors.New("offline device flow denied")
	}
	if _, err := c.GetToken(context.Background(), policy.TokenRequestOptions{}); err == nil || !called {
		t.Fatalf("cache miss did not enter device flow: %v", err)
	}
}

func TestCredentialScopeTenantCancellationAndInvalidToken(t *testing.T) {
	for _, options := range []policy.TokenRequestOptions{
		{Scopes: []string{"https://graph.microsoft.com/.default"}},
		{Scopes: []string{"Files.ReadWrite"}},
		{TenantID: "organizations"},
	} {
		c, _, _ := fakeCredential(t)
		if _, err := c.GetToken(context.Background(), options); err == nil {
			t.Fatalf("accepted options %#v", options)
		}
	}
	c, _, _ := fakeCredential(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetToken(ctx, policy.TokenRequestOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, result := range []public.AuthResult{
		{}, {Account: public.Account{HomeAccountID: "intended"}},
		{Account: public.Account{HomeAccountID: "intended"}, AccessToken: "token", ExpiresOn: time.Now().Add(-time.Hour)},
		authResult("other"),
	} {
		c.cache.accountID = "intended"
		if _, err := c.accessToken(result); err == nil {
			t.Fatal("accepted invalid authentication result")
		}
	}
}

func TestRequiresInteractionUsesOnlyRecognizedOAuthFailures(t *testing.T) {
	for _, code := range []string{"interaction_required", "login_required", "consent_required", "invalid_grant", "invalid_client", "temporarily_unavailable", ""} {
		for _, status := range []int{400, 401, 500} {
			err := msalerrors.CallErr{
				Resp: &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":` + marshalJSON(t, code) + `}`))},
				Err:  errors.New("untrusted response"),
			}
			want := status == 400 && (code == "interaction_required" || code == "login_required" || code == "consent_required" || code == "invalid_grant")
			if got := requiresInteraction(err); got != want {
				t.Fatalf("%s/%d: got %v, want %v", code, status, got, want)
			}
		}
	}
	if requiresInteraction(errors.New("bad cache: no token found")) || requiresInteraction(context.Canceled) {
		t.Fatal("used a broad fallback to interactive authentication")
	}
}

func TestNewRejectsMissingClientIDAndNeverPromptsWhenDisabled(t *testing.T) {
	dir := workDir(t)
	if _, err := New(context.Background(), config.Config{AuthDir: dir}, io.Discard); err == nil {
		t.Fatal("accepted missing public client ID")
	}
	_, err := New(context.Background(), config.Config{AuthDir: dir, MicrosoftClientID: "offline-client", InteractiveAuth: false}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "AUTH_INTERACTIVE=false") {
		t.Fatalf("noninteractive authentication did not fail explicitly: %v", err)
	}
}

func TestMSALDeviceCodeCacheReuseAndRefreshWithOfflineTransport(t *testing.T) {
	dir := workDir(t)
	var tokenCalls, deviceCalls int
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "login.microsoftonline.com" {
			t.Fatalf("unexpected authentication host %s", request.URL.Host)
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/.well-known/openid-configuration"):
			return jsonResponse(request, 200, `{
				"authorization_endpoint":"https://login.microsoftonline.com/consumers/oauth2/v2.0/authorize",
				"token_endpoint":"https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
				"device_authorization_endpoint":"https://login.microsoftonline.com/consumers/oauth2/v2.0/devicecode",
				"issuer":"https://login.microsoftonline.com/consumers/v2.0"}`), nil
		case strings.HasSuffix(request.URL.Path, "/devicecode"):
			deviceCalls++
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("client_id") != "offline-client" || !strings.Contains(request.Form.Get("scope"), fileScope) {
				t.Fatalf("wrong device-code form %v", request.Form)
			}
			return jsonResponse(request, 200, `{"device_code":"offline-device","user_code":"OFFLINE","verification_uri":"https://microsoft.com/devicelogin","expires_in":600,"interval":1,"message":"Enter OFFLINE at microsoft.com/devicelogin"}`), nil
		case strings.HasSuffix(request.URL.Path, "/token"):
			tokenCalls++
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if tokenCalls == 1 {
				if request.Form.Get("grant_type") != "device_code" || request.Form.Get("device_code") != "offline-device" {
					t.Fatalf("wrong device token form %v", request.Form)
				}
			} else if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "offline-refresh" {
				t.Fatalf("MSAL did not refresh the cached token: %v", request.Form)
			}
			payload := marshalJSON(t, map[string]any{
				"aud": "offline-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
				"iss": authority + "/v2.0", "tid": "consumers", "oid": "user", "sub": "user",
			})
			idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
			clientInfo := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"user","utid":"consumers"}`))
			return jsonResponse(request, 200, marshalJSON(t, map[string]any{
				"access_token": fmt.Sprintf("offline-token-%d", tokenCalls), "refresh_token": "offline-refresh",
				"token_type": "Bearer", "scope": fileScope, "expires_in": 3600, "ext_expires_in": 3600,
				"id_token": idToken, "client_info": clientInfo,
			})), nil
		default:
			t.Fatalf("unexpected MSAL request path %s", request.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})
	makeCredential := func(interactive bool) *credential {
		persistence, err := newTokenCache(dir, "offline-client")
		if err != nil {
			t.Fatal(err)
		}
		client, err := public.New("offline-client", public.WithAuthority(authority), public.WithCache(persistence),
			public.WithHTTPClient(&http.Client{Transport: transport}), public.WithInstanceDiscovery(false))
		if err != nil {
			t.Fatal(err)
		}
		return &credential{client: msalClient{client}, cache: persistence, interactive: interactive, stderr: io.Discard}
	}
	ctx := context.Background()
	first := makeCredential(true)
	token, err := first.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{fileScope}})
	if err != nil || token.Token != "offline-token-1" {
		t.Fatalf("official MSAL device-code flow: %v", err)
	}
	restarted := makeCredential(false)
	token, err = restarted.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{fileScope}})
	if err != nil || token.Token != "offline-token-1" || tokenCalls != 1 || deviceCalls != 1 {
		t.Fatalf("official MSAL cache reuse: error=%v, token requests=%d, device requests=%d", err, tokenCalls, deviceCalls)
	}
	token, err = restarted.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{fileScope}, Claims: `{"access_token":{"offline":null}}`})
	if err != nil || token.Token != "offline-token-2" || tokenCalls != 2 || deviceCalls != 1 {
		t.Fatalf("official MSAL refresh: error=%v, token requests=%d", err, tokenCalls)
	}
	again := makeCredential(false)
	token, err = again.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{fileScope}})
	if err != nil || token.Token != "offline-token-2" || tokenCalls != 2 {
		t.Fatalf("refreshed token was not persisted: %v", err)
	}
}
